package readers

import (
	"bytes"
	"encoding/xml"
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
)

type coberturaRoot struct {
	XMLName  xml.Name
	Sources  []string           `xml:"sources>source"`
	Packages *coberturaPackages `xml:"packages"`
}

type coberturaPackages struct {
	Packages []struct {
		Classes []coberturaClass `xml:"classes>class"`
	} `xml:"package"`
}

type coberturaClass struct {
	Name     string `xml:"name,attr"`
	Filename string `xml:"filename,attr"`
	Methods  []struct {
		Name      string          `xml:"name,attr"`
		Signature string          `xml:"signature,attr"`
		Hits      string          `xml:"hits,attr"`
		Lines     []coberturaLine `xml:"lines>line"`
	} `xml:"methods>method"`
	Lines []coberturaLine `xml:"lines>line"`
}

type coberturaLine struct {
	Number string `xml:"number,attr"`
	Hits   string `xml:"hits,attr"`
}

// ReadCobertura reads a Cobertura XML report. A file's lines are the lines of
// its classes; a method ran when its hits attribute is positive or, without
// one, when one of its lines ran.
func ReadCobertura(data []byte) (CoverageReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return CoverageReport{}, diagnostics
	}
	var root coberturaRoot
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		return CoverageReport{}, malformed("Cobertura report does not decode: %v", err)
	}
	if root.XMLName.Local != "coverage" || root.Packages == nil {
		return CoverageReport{}, malformed("Cobertura report root is %q without a packages element, want coverage with packages", root.XMLName.Local)
	}
	builder := newCoverageBuilder()
	for _, pkg := range root.Packages.Packages {
		for _, class := range pkg.Classes {
			if class.Filename == "" {
				return CoverageReport{}, malformed("Cobertura class without a filename")
			}
			file := builder.file(class.Filename)
			where := "Cobertura " + strconv.Quote(class.Filename)
			for _, line := range class.Lines {
				number, hits, diagnostics := coberturaLineValues(where, line)
				if diagnostics != nil {
					return CoverageReport{}, diagnostics
				}
				if diagnostics := file.line(number, hits); diagnostics != nil {
					return CoverageReport{}, diagnostics
				}
			}
			for _, method := range class.Methods {
				ran := false
				if method.Hits != "" {
					hits, err := strconv.ParseInt(method.Hits, 10, 64)
					if err != nil || hits < 0 {
						return CoverageReport{}, malformed("%s: method %q hits %q is not a count", where, method.Name, method.Hits)
					}
					ran = hits > 0
				}
				for _, line := range method.Lines {
					_, hits, diagnostics := coberturaLineValues(where, line)
					if diagnostics != nil {
						return CoverageReport{}, diagnostics
					}
					ran = ran || (method.Hits == "" && hits > 0)
				}
				file.function(class.Name+"\x00"+method.Name+"\x00"+method.Signature, ran)
			}
		}
	}
	sources := make([]string, 0, len(root.Sources))
	for _, source := range root.Sources {
		sources = append(sources, slashPath(source))
	}
	return builder.report(sources), nil
}

func coberturaLineValues(where string, line coberturaLine) (int, int64, []diag.Diagnostic) {
	number, err1 := strconv.Atoi(line.Number)
	hits, err2 := strconv.ParseInt(line.Hits, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, malformed("%s: line number %q hits %q do not parse", where, line.Number, line.Hits)
	}
	return number, hits, checkLine(where, number, hits)
}
