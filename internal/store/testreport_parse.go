package store

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"aira/internal/domain"
	"aira/internal/gate"
)

type parsedTestReport struct {
	Results  []domain.TestResult
	Complete bool
}

func parseGoTestJSON(data []byte) (parsedTestReport, error) {
	events, err := gate.DecodeGoTestJSONEvents(data)
	if err != nil {
		return parsedTestReport{}, errors.New("E_TESTREPORT_INVALID: " + err.Error())
	}
	started := map[string]bool{}
	terminal := map[string]bool{}
	paused := map[string]bool{}
	packageStarted := map[string]bool{}
	packageTerminal := map[string]bool{}
	packageRuns := map[string]int{}
	packageFailure := map[string]bool{}
	var results []domain.TestResult
	malformed := func(message string) (parsedTestReport, error) {
		return parsedTestReport{}, fmt.Errorf("E_TESTREPORT_INVALID: go-json %s", message)
	}
	for _, event := range events {
		if event.Test != "" && (event.Action == "start" || event.Action == "package-terminal") {
			return malformed("package event has test")
		}
		switch event.Action {
		case "start":
			if packageStarted[event.Package] || event.Test != "" {
				return malformed("invalid package start")
			}
			packageStarted[event.Package] = true
		case "run":
			key := event.Package + "\x00" + event.Test
			if !packageStarted[event.Package] || event.Test == "" || packageTerminal[event.Package] || started[key] {
				return malformed("invalid test start")
			}
			started[key] = true
			packageRuns[event.Package]++
		case "pause", "cont":
			key := event.Package + "\x00" + event.Test
			if !started[key] || terminal[key] || event.Test == "" || paused[key] == (event.Action == "pause") {
				return malformed("invalid test pause state")
			}
			paused[key] = event.Action == "pause"
		case "output", "bench":
			// Output is diagnostic context and does not change test state.
		case "pass", "fail", "skip":
			if event.Test != "" {
				key := event.Package + "\x00" + event.Test
				if !started[key] || terminal[key] || paused[key] {
					return malformed("invalid test terminal")
				}
				terminal[key] = true
				outcome := domain.OutcomePass
				switch event.Action {
				case "fail":
					outcome = domain.OutcomeFail
				case "skip":
					outcome = domain.OutcomeSkip
				}
				var duration *int64
				if event.Elapsed > 0 {
					ns := int64(event.Elapsed * 1e9)
					duration = &ns
				}
				results = append(results, domain.TestResult{Name: event.Package + "/" + event.Test, Outcome: outcome, DurationNS: duration, Message: event.Output})
				continue
			}
			if !packageStarted[event.Package] || packageTerminal[event.Package] {
				return malformed("invalid package terminal")
			}
			for key, isStarted := range started {
				if strings.HasPrefix(key, event.Package+"\x00") && isStarted && !terminal[key] {
					return malformed("package ended with open test")
				}
			}
			packageTerminal[event.Package] = true
			packageFailure[event.Package] = event.Action == "fail"
		default:
			return malformed("unknown action")
		}
	}
	if len(packageStarted) == 0 {
		return malformed("no package")
	}
	complete := true
	for pkg := range packageStarted {
		if !packageTerminal[pkg] {
			complete = false
		}
		if packageFailure[pkg] && packageRuns[pkg] == 0 {
			complete = false
		}
	}
	for key := range started {
		if !terminal[key] || paused[key] {
			complete = false
		}
	}
	return parsedTestReport{Results: results, Complete: complete}, nil
}

type junitRoot struct {
	XMLName xml.Name
	Tests   *int         `xml:"tests,attr"`
	Cases   []junitCase  `xml:"testcase"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Tests  *int         `xml:"tests,attr"`
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

type junitCase struct {
	Name      string       `xml:"name,attr"`
	Classname string       `xml:"classname,attr"`
	Time      string       `xml:"time,attr"`
	Failure   *junitDetail `xml:"failure"`
	Error     *junitDetail `xml:"error"`
	Skipped   *junitDetail `xml:"skipped"`
}

type junitDetail struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

func parseJUnitXML(data []byte) (parsedTestReport, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root junitRoot
	if err := decoder.Decode(&root); err != nil {
		return parsedTestReport{}, fmt.Errorf("E_TESTREPORT_INVALID: junit XML: %w", err)
	}
	for {
		extra, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return parsedTestReport{}, fmt.Errorf("E_TESTREPORT_INVALID: junit XML: %w", err)
		}
		if text, ok := extra.(xml.CharData); ok && strings.TrimSpace(string(text)) == "" {
			continue
		}
		return parsedTestReport{}, errors.New("E_TESTREPORT_INVALID: junit XML contains multiple documents")
	}
	if root.XMLName.Local != "testsuite" && root.XMLName.Local != "testsuites" {
		return parsedTestReport{}, fmt.Errorf("E_TESTREPORT_INVALID: junit root %q is not testsuite(s)", root.XMLName.Local)
	}
	var cases []junitCase
	declared := 0
	if root.XMLName.Local == "testsuite" && root.Tests != nil && *root.Tests >= 0 {
		declared += *root.Tests
	}
	cases = append(cases, root.Cases...)
	for _, suite := range root.Suites {
		count, suiteCases := junitSuiteData(suite)
		declared += count
		cases = append(cases, suiteCases...)
	}
	results := make([]domain.TestResult, 0, len(cases))
	for _, testcase := range cases {
		name := strings.TrimSpace(testcase.Name)
		if testcase.Classname != "" {
			name = strings.TrimSpace(testcase.Classname) + "/" + name
		}
		if name == "" {
			return parsedTestReport{}, errors.New("E_TESTREPORT_INVALID: junit testcase has no name")
		}
		var duration *int64
		if testcase.Time != "" {
			seconds, err := strconv.ParseFloat(testcase.Time, 64)
			if err != nil || seconds < 0 {
				return parsedTestReport{}, fmt.Errorf("E_TESTREPORT_INVALID: junit testcase time %q is invalid", testcase.Time)
			}
			ns := int64(seconds * 1e9)
			duration = &ns
		}
		outcome := domain.OutcomePass
		message := ""
		if testcase.Error != nil {
			outcome, message = domain.OutcomeError, junitDetailText(testcase.Error)
		} else if testcase.Failure != nil {
			outcome, message = domain.OutcomeFail, junitDetailText(testcase.Failure)
		} else if testcase.Skipped != nil {
			outcome, message = domain.OutcomeSkip, junitDetailText(testcase.Skipped)
		}
		results = append(results, domain.TestResult{Name: name, Outcome: outcome, DurationNS: duration, Message: message})
	}
	return parsedTestReport{Results: results, Complete: declared == 0 || len(cases) >= declared}, nil
}

func junitSuiteData(suite junitSuite) (int, []junitCase) {
	declared := 0
	if suite.Tests != nil && *suite.Tests >= 0 {
		declared = *suite.Tests
	}
	cases := append([]junitCase(nil), suite.Cases...)
	for _, child := range suite.Suites {
		count, childCases := junitSuiteData(child)
		declared += count
		cases = append(cases, childCases...)
	}
	return declared, cases
}

func junitDetailText(detail *junitDetail) string {
	if detail == nil {
		return ""
	}
	if detail.Message != "" {
		return detail.Message
	}
	return strings.TrimSpace(detail.Text)
}

func parseTestReport(format string, data []byte) (parsedTestReport, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "go-json":
		return parseGoTestJSON(data)
	case "junit":
		return parseJUnitXML(data)
	default:
		return parsedTestReport{}, fmt.Errorf("E_ARGUMENT_INVALID: unknown test report format %q", format)
	}
}
