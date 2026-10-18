package main

// RunResult is one restore and task, as the benchmarks record it.
type RunResult struct {
	Snapshot    string  `json:"snapshot"`
	Env         string  `json:"env"`
	Task        string  `json:"task"`
	Strategy    string  `json:"strategy"`
	Mode        string  `json:"mode"`
	Profile     string  `json:"profile"`
	OK          bool    `json:"ok"`
	Err         string  `json:"err,omitempty"`
	StateMs     float64 `json:"state_ms"`
	DownloadMs  float64 `json:"download_ms,omitempty"` // full only
	ResumedMs   float64 `json:"resumed_ms"`
	ConnectedMs float64 `json:"connected_ms"`
	AnsweredMs  float64 `json:"answered_ms"`
	GuestMs     float64 `json:"guest_ms"` // the task's own run time inside the guest
	Touched     int64   `json:"touched"`
	FromStage   int64   `json:"from_stage"`
	Demand      int64   `json:"demand"`
	Zero        int64   `json:"zero"`
	Staged      int64   `json:"staged"`
	Unused      int64   `json:"unused"`
	Planned     int     `json:"planned"`
	DemandWait  float64 `json:"demand_wait_ms"` // summed over all demand fetches
	Bytes       int64   `json:"bytes"`          // fetched from the store
	Requests    int64   `json:"requests"`       // GETs against the store
	MemFaults   int64   `json:"mem_faults,omitempty"`
	DiskReads   int64   `json:"disk_reads,omitempty"`

	Fork *ForkResult `json:"fork,omitempty"`
}

// ForkResult is a fork of a running VM and a restore of the fork.
type ForkResult struct {
	Snapshot  string     `json:"snapshot"`
	PauseMs   float64    `json:"pause_ms"`
	TotalMs   float64    `json:"total_ms"`
	DirtyMem  int        `json:"dirty_mem"`
	DirtyDisk int        `json:"dirty_disk"`
	Uploaded  int64      `json:"uploaded"` // bytes after compression
	Reused    int        `json:"reused"`
	New       int        `json:"new"`
	Restore   *RunResult `json:"restore,omitempty"`
}
