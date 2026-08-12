package store

import (
	"strings"
	"testing"

	"aira/internal/domain"
)

const completeGoJSON = `{"Action":"start","Package":"example/pkg"}
{"Action":"run","Package":"example/pkg","Test":"TestPass"}
{"Action":"pass","Package":"example/pkg","Test":"TestPass","Elapsed":0.001}
{"Action":"run","Package":"example/pkg","Test":"TestFail"}
{"Action":"fail","Package":"example/pkg","Test":"TestFail","Elapsed":0.002}
{"Action":"run","Package":"example/pkg","Test":"TestSkip"}
{"Action":"skip","Package":"example/pkg","Test":"TestSkip"}
{"Action":"pass","Package":"example/pkg"}
`

func TestParseGoTestJSONRoundTripAndTruncation(t *testing.T) {
	parsed, err := parseGoTestJSON([]byte(completeGoJSON))
	if err != nil || !parsed.Complete {
		t.Fatalf("complete parse = %#v, %v", parsed, err)
	}
	if len(parsed.Results) != 3 || parsed.Results[0].Outcome != domain.OutcomePass || parsed.Results[1].Outcome != domain.OutcomeFail || parsed.Results[2].Outcome != domain.OutcomeSkip {
		t.Fatalf("results = %#v", parsed.Results)
	}
	truncated := strings.TrimSuffix(completeGoJSON, `{"Action":"pass","Package":"example/pkg"}
`)
	truncated = strings.TrimSuffix(truncated, "\n")
	parsed, err = parseGoTestJSON([]byte(truncated))
	if err != nil || parsed.Complete || len(parsed.Results) != 3 {
		t.Fatalf("truncated parse = %#v, %v; want stored incomplete", parsed, err)
	}
	bad := completeGoJSON + `{"Action":"bogus","Package":"example/pkg"}
`
	if _, err := parseGoTestJSON([]byte(bad)); ErrorCode(err) != "E_TESTREPORT_INVALID" {
		t.Fatalf("bad action error = %v", err)
	}
}

func TestParseJUnitXMLNormalisesOutcomesAndClassname(t *testing.T) {
	data := `<testsuite tests="4"><testcase classname="pkg.A" name="Pass" time="0.5"/><testcase classname="pkg.A" name="Fail"><failure message="boom"/></testcase><testcase classname="pkg.A" name="Error"><error>bad</error></testcase><testcase classname="pkg.A" name="Skip"><skipped/></testcase></testsuite>`
	parsed, err := parseJUnitXML([]byte(data))
	if err != nil || !parsed.Complete || len(parsed.Results) != 4 {
		t.Fatalf("junit parse = %#v, %v", parsed, err)
	}
	if parsed.Results[0].Name != "pkg.A/Pass" || parsed.Results[1].Outcome != domain.OutcomeFail || parsed.Results[2].Outcome != domain.OutcomeError || parsed.Results[3].Outcome != domain.OutcomeSkip {
		t.Fatalf("junit results = %#v", parsed.Results)
	}
	if parsed.Results[0].DurationNS == nil || *parsed.Results[0].DurationNS != 500000000 {
		t.Fatalf("duration = %#v", parsed.Results[0].DurationNS)
	}
	incomplete, err := parseJUnitXML([]byte(`<testsuite tests="2"><testcase name="one"/></testsuite>`))
	if err != nil || incomplete.Complete {
		t.Fatalf("junit incomplete = %#v, %v", incomplete, err)
	}
	if _, err := parseJUnitXML([]byte(`<testsuite><testcase`)); ErrorCode(err) != "E_TESTREPORT_INVALID" {
		t.Fatalf("malformed junit error = %v", err)
	}
}

func TestParseJUnitClassnameIsPartOfTestIdentity(t *testing.T) {
	parsed, err := parseJUnitXML([]byte(`<testsuite tests="2"><testcase classname="pkg.one" name="Same"/><testcase classname="pkg.two" name="Same"><failure/></testcase></testsuite>`))
	if err != nil || len(parsed.Results) != 2 {
		t.Fatalf("parse = %#v, %v", parsed, err)
	}
	if parsed.Results[0].Name != "pkg.one/Same" || parsed.Results[1].Name != "pkg.two/Same" || parsed.Results[1].Outcome != domain.OutcomeFail {
		t.Fatalf("classname identity results = %#v", parsed.Results)
	}
}
