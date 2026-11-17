package garminconnect

// Exposes unexported helpers to the external test package (garminconnect_test).
// Being a _test.go file, this is compiled only by `go test` and adds nothing to
// the package's public API.
var (
	RangeChunks = rangeChunks
	DateParam   = dateParam
)
