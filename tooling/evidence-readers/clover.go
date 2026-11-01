package readers

import (
	"bytes"
	"encoding/xml"
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
)

type cloverRoot struct {
	XMLName xml.Name
	Project *struct {
		// Children keeps the files and packages of the project in document order.
		Children []cloverChild `xml:",any"`
	} `xml:"project"`
}

// cloverChild is a file or a package of a Clover project.
type cloverChild struct {
	XMLName xml.Name
	cloverFile
	Files []cloverFile `xml:"file"`
}

type cloverFile struct {
	Name    string `xml:"name,attr"`
	Path    string `xml:"path,attr"`
	Metrics *struct {
		Methods        string `xml:"methods,attr"`
		CoveredMethods string `xml:"coveredmethods,attr"`
	} `xml:"metrics"`
	Lines []struct {
		Num   string `xml:"num,attr"`
		Type  string `xml:"type,attr"`
		Name  string `xml:"name,attr"`
		Count string `xml:"count,attr"`
	} `xml:"line"`
}

// ReadClover reads a Clover XML report. A file's path is its path attribute,
// else its name. Its stmt and cond lines are its instrumented lines and its
// method lines its functions; a file without method lines keeps the methods
// and coveredmethods of its metrics.
func ReadClover(data []byte) (CoverageReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return CoverageReport{}, diagnostics
	}
	var root cloverRoot
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		return CoverageReport{}, malformed("Clover report does not decode: %v", err)
	}
	if root.XMLName.Local != "coverage" || root.Project == nil {
		return CoverageReport{}, malformed("Clover report root is %q without a project element, want coverage with project", root.XMLName.Local)
	}
	var files []cloverFile
	for _, child := range root.Project.Children {
		switch child.XMLName.Local {
		case "file":
			files = append(files, child.cloverFile)
		case "package":
			files = append(files, child.Files...)
		}
	}
	builder := newCoverageBuilder()
	for _, entry := range files {
		path := entry.Path
		if path == "" {
			path = entry.Name
		}
		if path == "" {
			return CoverageReport{}, malformed("Clover file without a path or a name")
		}
		file := builder.file(path)
		where := "Clover " + strconv.Quote(path)
		for _, line := range entry.Lines {
			number, err1 := strconv.Atoi(line.Num)
			hits, err2 := strconv.ParseInt(line.Count, 10, 64)
			if err1 != nil || err2 != nil {
				return CoverageReport{}, malformed("%s: line num %q count %q do not parse", where, line.Num, line.Count)
			}
			if diagnostics := checkLine(where, number, hits); diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
			switch line.Type {
			case "method":
				file.function(strconv.Itoa(number)+"\x00"+line.Name, hits > 0)
			case "stmt", "cond":
				file.line(number, hits)
			}
		}
		if entry.Metrics != nil && (entry.Metrics.Methods != "" || entry.Metrics.CoveredMethods != "") {
			total, diagnostics := count32(entry.Metrics.Methods, where+" methods")
			if diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
			covered, diagnostics := count32(entry.Metrics.CoveredMethods, where+" coveredmethods")
			if diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
			file.stateFunctions(Counts{Total: total, Covered: covered})
		}
	}
	return builder.report(nil), nil
}
