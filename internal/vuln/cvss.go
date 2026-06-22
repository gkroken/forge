package vuln

import (
	"math"
	"strings"
)

// cvssBaseScore parses a CVSS v3.0/v3.1 vector string and returns the base score
// (0–10) per the CVSS v3.1 specification formula. ok is false when the vector is
// not v3.x or is missing a required base metric. CVSS v4.0 uses a different
// formula and is not scored here (ok=false) — callers fall back to the curated
// severity label or store the raw vector and leave severity Unknown.
func cvssBaseScore(vector string) (float64, bool) {
	if !strings.HasPrefix(vector, "CVSS:3.0/") && !strings.HasPrefix(vector, "CVSS:3.1/") {
		return 0, false
	}
	m := map[string]string{}
	for _, part := range strings.Split(vector, "/")[1:] {
		if k, v, ok := strings.Cut(part, ":"); ok {
			m[k] = v
		}
	}

	av, ok1 := map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2}[m["AV"]]
	ac, ok2 := map[string]float64{"L": 0.77, "H": 0.44}[m["AC"]]
	ui, ok3 := map[string]float64{"N": 0.85, "R": 0.62}[m["UI"]]

	scope := m["S"]
	changed := scope == "C"
	var pr float64
	var ok4 bool
	if changed {
		pr, ok4 = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.5}[m["PR"]]
	} else {
		pr, ok4 = map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}[m["PR"]]
	}

	cia := map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
	c, ok5 := cia[m["C"]]
	i, ok6 := cia[m["I"]]
	a, ok7 := cia[m["A"]]

	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) || (scope != "U" && scope != "C") {
		return 0, false
	}

	iscBase := 1 - (1-c)*(1-i)*(1-a)
	var impact float64
	if changed {
		impact = 7.52*(iscBase-0.029) - 3.25*math.Pow(iscBase-0.02, 15)
	} else {
		impact = 6.42 * iscBase
	}
	if impact <= 0 {
		return 0, true
	}

	expl := 8.22 * av * ac * pr * ui
	var base float64
	if changed {
		base = math.Min(1.08*(expl+impact), 10)
	} else {
		base = math.Min(expl+impact, 10)
	}
	return roundup(base), true
}

// roundup implements the CVSS v3.1 Roundup function: round up to one decimal
// place, using integer arithmetic to avoid floating-point boundary errors.
func roundup(input float64) float64 {
	intInput := int(math.Round(input * 100000))
	if intInput%10000 == 0 {
		return float64(intInput) / 100000.0
	}
	return (math.Floor(float64(intInput)/10000) + 1) / 10.0
}
