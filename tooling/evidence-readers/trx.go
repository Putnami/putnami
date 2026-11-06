package readers

import (
	"bytes"
	"encoding/xml"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

// trxRun is the part of a TRX report the reader uses.
type trxRun struct {
	XMLName xml.Name `xml:"TestRun"`
	Times   struct {
		Start  string `xml:"start,attr"`
		Finish string `xml:"finish,attr"`
	} `xml:"Times"`
	Results     []trxResult `xml:"Results>UnitTestResult"`
	Definitions []struct {
		ID     string `xml:"id,attr"`
		Method struct {
			CodeBase  string `xml:"codeBase,attr"`
			ClassName string `xml:"className,attr"`
		} `xml:"TestMethod"`
	} `xml:"TestDefinitions>UnitTest"`
	Counters struct {
		Total       string `xml:"total,attr"`
		Failed      string `xml:"failed,attr"`
		Error       string `xml:"error,attr"`
		Timeout     string `xml:"timeout,attr"`
		Aborted     string `xml:"aborted,attr"`
		NotExecuted string `xml:"notExecuted,attr"`
	} `xml:"ResultSummary>Counters"`
}

type trxResult struct {
	TestID   string `xml:"testId,attr"`
	TestName string `xml:"testName,attr"`
	Duration string `xml:"duration,attr"`
	Outcome  string `xml:"outcome,attr"`
	Error    struct {
		Message    string `xml:"Message"`
		StackTrace string `xml:"StackTrace"`
	} `xml:"Output>ErrorInfo"`
}

// TRX outcomes, by the status they give a case. An outcome in failedAsError
// fails the case as an error: it did not finish.
var (
	trxPassed        = map[string]bool{"Passed": true, "PassedButRunAborted": true, "Completed": true, "Warning": true}
	trxFailedAsError = map[string]bool{"Error": true, "Timeout": true, "Aborted": true}
	trxSkipped       = map[string]bool{"NotExecuted": true, "Inconclusive": true, "NotRunnable": true, "Pending": true, "Disconnected": true, "InProgress": true}
)

// trxDuration is a TRX duration: hours, minutes and seconds with up to seven
// decimals, such as 00:00:00.0123456.
var trxDuration = regexp.MustCompile(`^(\d+):(\d{2}):(\d{2}(?:\.\d+)?)$`)

// ReadTRX reads a TRX report, the test results file dotnet test writes with
// --logger trx, whose root is TestRun. Each top-level UnitTestResult of its
// Results is one case, in document order; the inner results of a data-driven
// test are not cases. A case's class name and suite come from the TestMethod
// of the UnitTest its testId names: the class name cut at its first comma, and
// one suite named by the file name of the test assembly, its codeBase.
func ReadTRX(data []byte) (TestReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return TestReport{}, diagnostics
	}
	var run trxRun
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&run); err != nil {
		return TestReport{}, malformed("TRX report does not decode: %v", err)
	}
	totals, diagnostics := trxTotals(run)
	if diagnostics != nil {
		return TestReport{}, diagnostics
	}
	type definition struct {
		classname string
		suites    []Suite
	}
	definitions := make(map[string]definition, len(run.Definitions))
	suites := map[string][]Suite{}
	for _, d := range run.Definitions {
		classname, _, _ := strings.Cut(d.Method.ClassName, ",")
		assembly := path.Base(slashPath(d.Method.CodeBase))
		if d.Method.CodeBase == "" {
			assembly = ""
		}
		if _, seen := suites[assembly]; !seen && assembly != "" {
			suites[assembly] = []Suite{{Name: assembly}}
		}
		definitions[d.ID] = definition{classname: strings.TrimSpace(classname), suites: suites[assembly]}
	}
	report := TestReport{Totals: totals, Cases: make([]TestCase, 0, len(run.Results))}
	for _, result := range run.Results {
		testCase := TestCase{Name: result.TestName, DurationMs: trxDurationMs(result.Duration)}
		if d, ok := definitions[result.TestID]; ok {
			testCase.Classname, testCase.Suites = d.classname, d.suites
		}
		switch {
		case trxPassed[result.Outcome]:
			testCase.Status = StatusPassed
		case result.Outcome == "Failed", trxFailedAsError[result.Outcome]:
			testCase.Status = StatusFailed
			testCase.Failure = &Failure{Error: trxFailedAsError[result.Outcome], Message: result.Error.Message, Body: result.Error.StackTrace}
		case trxSkipped[result.Outcome]:
			testCase.Status = StatusSkipped
			testCase.Skipped = &Skipped{Message: result.Error.Message}
		default:
			return TestReport{}, malformed("TRX result %q has the unknown outcome %q", result.TestName, result.Outcome)
		}
		report.Cases = append(report.Cases, testCase)
	}
	return report, nil
}

// trxTotals reads the ResultSummary counters; errors count the error, timeout
// and aborted results, and the time is the run's start to its finish.
func trxTotals(run trxRun) (Totals, []diag.Diagnostic) {
	counters := run.Counters
	values := map[string]int{}
	for name, value := range map[string]string{
		"total": counters.Total, "failed": counters.Failed, "error": counters.Error,
		"timeout": counters.Timeout, "aborted": counters.Aborted, "notExecuted": counters.NotExecuted,
	} {
		parsed, diagnostics := count32(value, "TRX counter "+name)
		if diagnostics != nil {
			return Totals{}, diagnostics
		}
		values[name] = parsed
	}
	totals := Totals{
		Tests:    values["total"],
		Failures: values["failed"],
		Errors:   values["error"] + values["timeout"] + values["aborted"],
		Skipped:  values["notExecuted"],
	}
	start, startErr := time.Parse(time.RFC3339Nano, run.Times.Start)
	finish, finishErr := time.Parse(time.RFC3339Nano, run.Times.Finish)
	if startErr == nil && finishErr == nil && finish.After(start) {
		totals.Time = finish.Sub(start).Seconds()
	}
	return totals, nil
}

// trxDurationMs converts a TRX duration to whole milliseconds; a duration that
// does not read so is 0.
func trxDurationMs(value string) int64 {
	m := trxDuration.FindStringSubmatch(value)
	if m == nil {
		return 0
	}
	hours, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	minutes, _ := strconv.ParseFloat(m[2], 64)
	secs, _ := strconv.ParseFloat(m[3], 64)
	return durationMs(hours*3600 + minutes*60 + secs)
}
