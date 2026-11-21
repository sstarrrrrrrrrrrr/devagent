package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Skill 是一个可加载的技能：skills/<name>/SKILL.md。
//
// 索引只保留 frontmatter 里的 name / description（进 system prompt 做索引），
// 正文由模型在需要时通过 load_skill 拉取，附件通过 read_skill_file 按需读取，
// 实现「索引常驻、正文按需、附件再按需」的渐进披露。
type Skill struct {
	Name        string // 与目录名一致
	Description string // 触发条件说明，模型据此判断何时加载
}

var (
	skillsOnce  sync.Once
	skillsCache []Skill
)

// skillsRoot 返回技能目录的绝对路径（项目根下的 skills/）。
func skillsRoot() string {
	return filepath.Join(workdirRoot(), "skills")
}

// ListSkills 返回技能索引（渐进披露的 L1）。
//
// 启动后只扫描一次 skills/ 目录，且每个 SKILL.md 只读到 frontmatter 结束为止，
// 正文一个字都不读。新增/删除技能目录需要重启；改正文即时生效
// （load_skill 每次现读）。
func ListSkills() []Skill {
	skillsOnce.Do(func() {
		skillsCache = scanSkills()
	})
	return skillsCache
}

// scanSkills 扫描 skills/ 目录，解析所有 SKILL.md 的 frontmatter。
// 单个 skill 解析失败只打警告不阻塞其他 skill；
// 目录不存在时返回空列表（skill 功能整体静默降级）。
func scanSkills() []Skill {
	entries, err := os.ReadDir(skillsRoot())
	if err != nil {
		return nil // 没有 skills/ 目录 → 视为没有技能
	}

	var result []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skill, err := loadSkill(entry.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  加载技能 %q 失败: %v\n", entry.Name(), err)
			continue
		}
		result = append(result, skill)
	}

	// 按名字排序，保证 system prompt 里的技能列表顺序稳定
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// loadSkill 读取并解析单个 skill 的 SKILL.md，只解析 frontmatter。
func loadSkill(name string) (Skill, error) {
	frontmatter, err := readFrontmatter(filepath.Join(skillsRoot(), name, "SKILL.md"))
	if err != nil {
		return Skill{}, err
	}

	fields := parseFrontmatter(frontmatter)
	skill := Skill{Name: name, Description: fields["description"]}

	// name 必须和目录名一致，防止 frontmatter 里写错导致索引和加载对不上
	if parsed := fields["name"]; parsed != "" {
		skill.Name = parsed
	}
	if skill.Name != name {
		return Skill{}, fmt.Errorf("frontmatter name %q 与目录名 %q 不一致", skill.Name, name)
	}
	if skill.Description == "" {
		return Skill{}, fmt.Errorf("缺少 description 字段（模型靠它判断何时使用该技能）")
	}

	return skill, nil
}

// readFrontmatter 只读到第二条 "---" 为止，正文一个字不读。
func readFrontmatter(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("读取 SKILL.md 失败: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	started, closed := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !started {
			line = strings.TrimPrefix(line, "\ufeff") // 容忍 BOM
		}
		if strings.TrimSpace(line) == "---" {
			if started {
				closed = true
				break
			}
			started = true
			continue
		}
		if started {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("读取 SKILL.md 失败: %w", err)
	}
	if !closed {
		return "", fmt.Errorf("SKILL.md 缺少 --- 包裹的 frontmatter")
	}
	return b.String(), nil
}

// splitFrontmatter 把 "---" 包裹的 YAML 头和正文分开。
// 没有 frontmatter 时返回 ("", 原文)。
func splitFrontmatter(content string) (frontmatter, body string) {
	trimmed := strings.TrimPrefix(content, "\ufeff") // 容忍 BOM
	if !strings.HasPrefix(trimmed, "---") {
		return "", content
	}

	rest := strings.TrimPrefix(trimmed, "---")
	// 去掉 "---" 行尾的换行
	if idx := strings.IndexByte(rest, '\n'); idx >= 0 {
		rest = rest[idx+1:]
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", content // 没有闭合标记，整体当正文处理
	}

	return rest[:end], strings.TrimPrefix(rest[end+4:], "\n")
}

// parseFrontmatter 解析 frontmatter 里的 key: value。
//
// 支持 YAML 的块标量（`description: >` 折叠成空格、`|` 保留换行）和缩进续行：
// 多行 description 只取首行的话，丢掉的往往正是触发词。
func parseFrontmatter(frontmatter string) map[string]string {
	fields := make(map[string]string)
	key := ""
	blockStyle := "" // "" 单行；">" 折叠；"|" 保留换行
	var block []string

	flush := func() {
		if key == "" {
			return
		}
		switch blockStyle {
		case ">":
			fields[key] = strings.TrimSpace(strings.Join(block, " "))
		case "|":
			fields[key] = strings.TrimSpace(strings.Join(block, "\n"))
		default:
			if joined := strings.TrimSpace(strings.Join(block, " ")); joined != "" {
				fields[key] = strings.TrimSpace(fields[key] + " " + joined)
			}
		}
		block, blockStyle = nil, ""
	}

	for _, line := range strings.Split(frontmatter, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// 缩进行是上一个 key 的续行
		if line[0] == ' ' || line[0] == '\t' {
			if key != "" {
				block = append(block, trimmed)
			}
			continue
		}
		k, v, ok := cutField(line)
		if !ok {
			continue
		}
		flush()
		key = k
		switch v {
		case ">", ">-", ">+":
			blockStyle = ">"
		case "|", "|-", "|+":
			blockStyle = "|"
		default:
			fields[k] = v
		}
	}
	flush()
	return fields
}

// cutField 解析 "key: value" 形式的一行。
func cutField(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	idx := strings.IndexByte(line, ':')
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

// skillBody 现读 SKILL.md 并返回去掉 frontmatter 后的正文（L2）。
// 现读而不是启动时缓存，这样改正文存盘即生效。
func skillBody(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(skillsRoot(), name, "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("读取 SKILL.md 失败: %w", err)
	}
	_, body := splitFrontmatter(string(data))
	return body, nil
}

// skillExists 判断技能是否在索引里。
func skillExists(name string) bool {
	for _, s := range ListSkills() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// skillNames 返回全部技能名，用于报错时让模型自纠。
func skillNames() []string {
	skills := ListSkills()
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}

// skillFiles 列出技能目录下的附件（不含 SKILL.md），用于报错时让模型自纠。
func skillFiles(name string) []string {
	dir := filepath.Join(skillsRoot(), name)
	var files []string
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "SKILL.md" {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	if len(files) > 20 {
		files = append(files[:20], "…")
	}
	return files
}

// SkillListPrompt 把技能索引渲染成 system prompt 的一节。
// 没有任何技能时返回空串，调用方据此决定是否拼接该节。
func SkillListPrompt() string {
	skills := ListSkills()
	if len(skills) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n以下是当前可用的技能（skill）。技能是针对特定任务类型的详细作业指令，")
	b.WriteString("当用户的请求命中某个技能的描述时，你必须先调用 load_skill 工具读取该技能的完整内容，")
	b.WriteString("然后严格按其指令执行。技能列表：")
	for _, s := range skills {
		fmt.Fprintf(&b, "\n- %s：%s", s.Name, s.Description)
	}
	b.WriteString("\n技能正文里点名引用的附件（如 references/xxx.md），用 read_skill_file 按需读取，不要提前读、也不要一次读多个。")
	b.WriteString("\n注意：技能的指令是对常规工具使用规则的补充，不能替代安全确认——")
	b.WriteString("技能要求执行命令或写文件时，安全审批流程照常生效。")
	return b.String()
}

// LoadSkillTool 让模型按需读取技能正文的只读工具（L2）。
var LoadSkillTool = Tool{
	Name:        "load_skill",
	Description: "读取指定技能（skill）的完整指令内容。调用前先确认系统提示词的技能列表里有这个名字，不要凭空猜测技能名",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill_name": map[string]any{
				"type":        "string",
				"description": "技能名称，必须与技能列表里的名字完全一致",
			},
		},
		"required":             []string{"skill_name"},
		"additionalProperties": false,
	},
	// 只读 + 固定目录 → L0Safe
	RiskLevel:    L0Safe,
	ExplicitRisk: true,
	Execute: func(ctx context.Context, args map[string]any) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		nameValue, ok := args["skill_name"].(string)
		if !ok || strings.TrimSpace(nameValue) == "" {
			return "", fmt.Errorf("skill_name 参数必须是非空字符串")
		}
		name := strings.TrimSpace(nameValue)

		// 不允许路径穿越：只取最后一段作为技能名
		name = filepath.Base(filepath.Clean(name))

		// 先确认技能存在，不存在时回可用列表帮模型自纠
		if !skillExists(name) {
			return "", fmt.Errorf("技能 %q 不存在，可用技能: %v", name, skillNames())
		}

		body, err := skillBody(name)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(body) == "" {
			return "", fmt.Errorf("技能 %q 的 SKILL.md 没有正文内容", name)
		}
		return body, nil
	},
}

// ReadSkillFileTool 让模型按需读取技能目录下的单个附件（L3）。
// 和 load_skill 的唯一区别是路径：L2 读 SKILL.md 本体，L3 读正文点名的那一个附件。
var ReadSkillFileTool = Tool{
	Name:        "read_skill_file",
	Description: "读取某个技能目录下的附件文件（如 references/xxx.md、templates/xxx.html）。只读，路径相对于技能目录，不能跳出该目录；仅在技能正文点名要读某个附件时调用",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill_name": map[string]any{
				"type":        "string",
				"description": "技能名称，必须与技能列表里的名字完全一致",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "附件路径，相对于技能目录，如 references/style-guide.md",
			},
		},
		"required":             []string{"skill_name", "path"},
		"additionalProperties": false,
	},
	// 只读 + 固定目录 → L0Safe
	RiskLevel:    L0Safe,
	ExplicitRisk: true,
	Execute: func(ctx context.Context, args map[string]any) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		nameValue, ok := args["skill_name"].(string)
		if !ok || strings.TrimSpace(nameValue) == "" {
			return "", fmt.Errorf("skill_name 参数必须是非空字符串")
		}
		name := filepath.Base(filepath.Clean(strings.TrimSpace(nameValue)))
		if !skillExists(name) {
			return "", fmt.Errorf("技能 %q 不存在，可用技能: %v", name, skillNames())
		}

		pathValue, ok := args["path"].(string)
		if !ok || strings.TrimSpace(pathValue) == "" {
			return "", fmt.Errorf("path 参数必须是非空字符串")
		}
		clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(pathValue)))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path 必须是技能目录内的相对路径，收到 %q", pathValue)
		}

		dir := filepath.Join(skillsRoot(), name)
		full := filepath.Join(dir, clean)
		// 双保险：确认拼接结果没有跳出技能目录（如软链接、异常分隔符）
		if rel, err := filepath.Rel(dir, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path 不能跳到技能目录之外: %q", pathValue)
		}

		data, err := os.ReadFile(full)
		if err != nil {
			return "", fmt.Errorf("读取 %s/%s 失败（可用附件: %v）: %w", name, filepath.ToSlash(clean), skillFiles(name), err)
		}
		return string(data), nil
	},
}
