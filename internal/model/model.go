package model
type Metric struct { Name, Help string; Labels map[string]string; Value float64 }
type Snapshot struct { Vendor string; Metrics []Metric; Resources int }
