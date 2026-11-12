package db

import "encoding/json"

// IssueStages are the stages of a task in order, with their labels.
var IssueStages = []struct{ Key, Label string }{
	{"new", "Новая"},
	{"research", "Исследование"},
	{"planned", "Есть план"},
	{"implementing", "Реализация"},
	{"implemented", "Готово к MR"},
	{"review", "MR на ревью"},
	{"merged", "MR влит"},
	{"done", "Закрыта"},
}

// StageLabel is the human label of a stage.
func StageLabel(key string) string {
	for _, s := range IssueStages {
		if s.Key == key {
			return s.Label
		}
	}
	return key
}

// ValidStage reports a known stage key.
func ValidStage(key string) bool {
	for _, s := range IssueStages {
		if s.Key == key {
			return true
		}
	}
	return false
}

// LinkedMR is an MR linked to a task.
type LinkedMR struct {
	Host         string `json:"host"`
	ProjectPath  string `json:"project_path"`
	IID          int64  `json:"iid"`
	Title        string `json:"title"`
	State        string `json:"state"` // opened | merged | closed
	WebURL       string `json:"web_url"`
	SourceBranch string `json:"source_branch"`
	Via          string `json:"via"`             // closes | related | branch
	MRID         int64  `json:"mr_id,omitempty"` // the MR in the dashboard (0 = not there)
}

// IssueNote is a comment of a task.
type IssueNote struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// SetIssueStage stores the computed stage; a stage set by hand is left alone.
func (d *DB) SetIssueStage(id int64, stage string) error {
	_, err := d.sql.Exec("UPDATE issues SET stage = ? WHERE id = ? AND stage_manual = 0", stage, id)
	return err
}

// SetIssueStageManual sets the stage by hand, or (manual = false) gives it back to the automatic computation.
func (d *DB) SetIssueStageManual(id int64, stage string, manual bool) error {
	flag := 0
	if manual {
		flag = 1
	}
	_, err := d.sql.Exec("UPDATE issues SET stage = ?, stage_manual = ? WHERE id = ?", stage, flag, id)
	return err
}

// SetIssueLinks stores the linked MRs and the comments of a task.
func (d *DB) SetIssueLinks(id int64, mrs []LinkedMR, notes []IssueNote) error {
	if mrs == nil {
		mrs = []LinkedMR{}
	}
	if notes == nil {
		notes = []IssueNote{}
	}
	m, _ := json.Marshal(mrs)
	n, _ := json.Marshal(notes)
	_, err := d.sql.Exec("UPDATE issues SET related_mrs = ?, notes_json = ? WHERE id = ?", string(m), string(n), id)
	return err
}

// IssueLinks returns the stored linked MRs and comments of a task.
func (d *DB) IssueLinks(id int64) ([]LinkedMR, []IssueNote) {
	var m, n string
	_ = d.sql.QueryRow("SELECT related_mrs, notes_json FROM issues WHERE id = ?", id).Scan(&m, &n)
	var mrs []LinkedMR
	var notes []IssueNote
	_ = json.Unmarshal([]byte(m), &mrs)
	_ = json.Unmarshal([]byte(n), &notes)
	return mrs, notes
}

// IssuesOfMR returns the tasks that list the MR among their linked MRs (by GitLab reference).
func (d *DB) IssuesOfMR(host, projectPath string, iid int64) ([]Issue, error) {
	rows, err := d.sql.Query("SELECT "+issueColumns+" FROM issues i WHERE EXISTS (SELECT 1 FROM json_each(i.related_mrs) j WHERE json_extract(j.value, '$.iid') = ? AND json_extract(j.value, '$.project_path') = ? AND (json_extract(j.value, '$.host') = ? OR json_extract(j.value, '$.host') = '')) ORDER BY i.id", iid, projectPath, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}
