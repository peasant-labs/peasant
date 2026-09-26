package teststream

import (
	"io"
	"regexp"
	"time"
)

// Compile-time pins for the shared stream library's exported contract (IP-A).
// A rename, removal, or retype breaks this file's build; the runtime fixture in
// contract_test.go adds struct tags and field order.

var (
	_ = Event{Action: "", Package: "", Test: "", Elapsed: 0, Output: "", Time: ""}
	_ = Record{Package: "", Test: "", Elapsed: 0, Skipped: false, Failed: false}
	_ = ReportOptions{Top: 0, FamilyRe: (*regexp.Regexp)(nil), NoFamilies: false, WarnPct: 0}
)

var (
	_ func(io.Reader) ([]Record, error)              = ParseStream
	_ func([]Record) []Record                        = TopLevel
	_ func([]Record) []Record                        = Failing
	_ func(io.Writer, []Record, ReportOptions) error = Report
)

// time.Duration is the record's elapsed unit; assert the conversion stays valid.
var _ time.Duration = Record{}.Elapsed
