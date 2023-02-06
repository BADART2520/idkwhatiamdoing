package model

// Used in thc client TUI
type Context struct {
	Target string
	From   string
	Limit  int
	// JsonOutput is a flag that determines whether the output should be in JSON format.
	JsonOutput bool
	// Latency is a flag that outputs only stats of a measurement
	Latency bool
}
