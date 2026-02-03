package model

import "time"

type Capture struct {
	Kind       string    `json:"kind"`
	Phase      string    `json:"phase"`
	File       string    `json:"file,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error,omitempty"`
}

type Frame struct {
	Name string `json:"name"`
	File string `json:"file,omitempty"`
	Line int64  `json:"line,omitempty"`
}

type Function struct {
	Frame
	Flat int64 `json:"flat"`
	Cum  int64 `json:"cum"`
}

type Stack struct {
	Frames []Frame `json:"frames"`
	Value  int64   `json:"value"`
}

type Metric struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	SampleType string     `json:"sample_type"`
	Unit       string     `json:"unit"`
	Scope      string     `json:"scope"`
	Total      int64      `json:"total"`
	Before     *int64     `json:"before,omitempty"`
	Top        []Function `json:"top,omitempty"`
	Stacks     []Stack    `json:"stacks,omitempty"`
	Note       string     `json:"note,omitempty"`
}

type Finding struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Evidence   []string `json:"evidence"`
	Hypothesis string   `json:"hypothesis,omitempty"`
	NextStep   string   `json:"next_step"`
}

type Analysis struct {
	Metrics  []Metric  `json:"metrics"`
	Findings []Finding `json:"findings"`
	Notes    []string  `json:"notes"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Session struct {
	Version    int           `json:"version"`
	ID         string        `json:"id"`
	Target     string        `json:"target"`
	Goal       string        `json:"goal,omitempty"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Duration   time.Duration `json:"duration_ns"`
	Captures   []Capture     `json:"captures"`
	Analysis   Analysis      `json:"analysis"`
	Messages   []Message     `json:"messages,omitempty"`
	SourceRoot string        `json:"source_root,omitempty"`
}
