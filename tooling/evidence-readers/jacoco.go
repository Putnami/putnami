package readers

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
)

type jacocoPackage struct {
	// Name is the package's path, slash-separated; empty for the default
	// package.
	Name        string `xml:"name,attr"`
	SourceFiles []struct {
		Name  string `xml:"name,attr"`
		Lines []struct {
			Nr string `xml:"nr,attr"`
			Mi string `xml:"mi,attr"`
			Ci string `xml:"ci,attr"`
		} `xml:"line"`
		Counters []jacocoCounter `xml:"counter"`
	} `xml:"sourcefile"`
}

type jacocoCounter struct {
	Type    string `xml:"type,attr"`
	Missed  string `xml:"missed,attr"`
	Covered string `xml:"covered,attr"`
}

// ReadJaCoCo reads a JaCoCo XML report, as the JaCoCo Maven plugin, the
// Gradle jacoco plugin and Kover write it. Each sourcefile element is one
// file, at its package path and source file name. A line with a covered
// instruction is covered, with one hit, since JaCoCo records no execution
// count; a line with missed instructions only is uncovered. A source file's
// LINE and METHOD counters are its stated totals.
func ReadJaCoCo(data []byte) (CoverageReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return CoverageReport{}, diagnostics
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	builder := newCoverageBuilder()
	rooted := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return CoverageReport{}, malformed("JaCoCo report does not decode: %v", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if !rooted {
			if start.Name.Local != "report" {
				return CoverageReport{}, malformed("JaCoCo report root is %q, want report", start.Name.Local)
			}
			rooted = true
			continue
		}
		switch start.Name.Local {
		case "group":
			// report-aggregate writes a group per module, and groups nest:
			// their packages are read where they stand.
		case "package":
			var pkg jacocoPackage
			if err := decoder.DecodeElement(&pkg, &start); err != nil {
				return CoverageReport{}, malformed("JaCoCo report does not decode: %v", err)
			}
			if diagnostics := readJaCoCoPackage(builder, pkg); diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
		default:
			if err := decoder.Skip(); err != nil {
				return CoverageReport{}, malformed("JaCoCo report does not decode: %v", err)
			}
		}
	}
	if !rooted {
		return CoverageReport{}, malformed("JaCoCo report has no root element")
	}
	return builder.report(nil), nil
}

func readJaCoCoPackage(builder *coverageBuilder, pkg jacocoPackage) []diag.Diagnostic {
	for _, source := range pkg.SourceFiles {
		if source.Name == "" {
			return malformed("JaCoCo sourcefile without a name in package %q", pkg.Name)
		}
		path := source.Name
		if pkg.Name != "" {
			path = pkg.Name + "/" + source.Name
		}
		file := builder.file(path)
		where := "JaCoCo " + strconv.Quote(path)
		for _, line := range source.Lines {
			number, err1 := strconv.Atoi(line.Nr)
			missed, err2 := count(line.Mi)
			covered, err3 := count(line.Ci)
			if err1 != nil || err2 != nil || err3 != nil {
				return malformed("%s: line nr %q mi %q ci %q do not parse", where, line.Nr, line.Mi, line.Ci)
			}
			if missed < 0 || covered < 0 {
				return malformed("%s: line %d has a negative instruction count", where, number)
			}
			if missed == 0 && covered == 0 {
				continue
			}
			hits := int64(0)
			if covered > 0 {
				hits = 1
			}
			if diagnostics := checkLine(where, number, hits); diagnostics != nil {
				return diagnostics
			}
			if diagnostics := file.line(number, hits); diagnostics != nil {
				return diagnostics
			}
		}
		for _, counter := range source.Counters {
			if counter.Type != "LINE" && counter.Type != "METHOD" {
				continue
			}
			missed, diagnostics := count32(counter.Missed, where+" "+counter.Type+" missed")
			if diagnostics != nil {
				return diagnostics
			}
			covered, diagnostics := count32(counter.Covered, where+" "+counter.Type+" covered")
			if diagnostics != nil {
				return diagnostics
			}
			counts := Counts{Total: missed + covered, Covered: covered}
			if counter.Type == "LINE" {
				file.stateLines(counts)
			} else {
				file.stateFunctions(counts)
			}
		}
	}
	return nil
}

func count(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.ParseInt(value, 10, 64)
}
