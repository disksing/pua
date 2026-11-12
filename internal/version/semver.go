// Package version implements the strict SemVer subset used by component
// releases and update manifests. It deliberately does not accept a leading v;
// Git tags carry v, runtime product versions do not.
package version

import (
	"errors"
	"strconv"
	"strings"
)

type Identifier struct {
	Value   string
	Numeric bool
}

type Version struct {
	Major      uint64
	Minor      uint64
	Patch      uint64
	Prerelease []Identifier
	Metadata   []string
	original   string
}

func Parse(input string) (Version, error) {
	input = strings.TrimSpace(input)
	if input == "" || strings.HasPrefix(input, "v") {
		return Version{}, errors.New("version must be SemVer without a leading v")
	}
	mainAndMetadata := strings.SplitN(input, "+", 2)
	if len(mainAndMetadata) > 2 {
		return Version{}, errors.New("version has multiple build metadata separators")
	}
	main := mainAndMetadata[0]
	var metadata []string
	if len(mainAndMetadata) == 2 {
		var err error
		metadata, err = parseTextIdentifiers(mainAndMetadata[1], false)
		if err != nil {
			return Version{}, err
		}
	}
	mainAndPre := strings.SplitN(main, "-", 2)
	core := strings.Split(mainAndPre[0], ".")
	if len(core) != 3 {
		return Version{}, errors.New("version core must contain major.minor.patch")
	}
	values := make([]uint64, 3)
	for index, part := range core {
		value, err := parseNumeric(part)
		if err != nil {
			return Version{}, err
		}
		values[index] = value
	}
	var prerelease []Identifier
	if len(mainAndPre) == 2 {
		parts, err := parseTextIdentifiers(mainAndPre[1], true)
		if err != nil {
			return Version{}, err
		}
		prerelease = make([]Identifier, 0, len(parts))
		for _, part := range parts {
			numeric := isNumeric(part)
			if numeric && len(part) > 1 && part[0] == '0' {
				return Version{}, errors.New("numeric prerelease identifier has a leading zero")
			}
			prerelease = append(prerelease, Identifier{Value: part, Numeric: numeric})
		}
	}
	return Version{Major: values[0], Minor: values[1], Patch: values[2], Prerelease: prerelease, Metadata: metadata, original: input}, nil
}

func MustParse(input string) Version {
	result, err := Parse(input)
	if err != nil {
		panic(err)
	}
	return result
}

func (v Version) String() string { return v.original }

func Compare(left, right Version) int {
	for _, pair := range [][2]uint64{{left.Major, right.Major}, {left.Minor, right.Minor}, {left.Patch, right.Patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.Prerelease) == 0 && len(right.Prerelease) > 0 {
		return 1
	}
	if len(left.Prerelease) > 0 && len(right.Prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(left.Prerelease) && index < len(right.Prerelease); index++ {
		leftID, rightID := left.Prerelease[index], right.Prerelease[index]
		if leftID.Value == rightID.Value {
			continue
		}
		if leftID.Numeric && !rightID.Numeric {
			return -1
		}
		if !leftID.Numeric && rightID.Numeric {
			return 1
		}
		if leftID.Numeric {
			leftValue, _ := strconv.ParseUint(leftID.Value, 10, 64)
			rightValue, _ := strconv.ParseUint(rightID.Value, 10, 64)
			if leftValue < rightValue {
				return -1
			}
			return 1
		}
		if leftID.Value < rightID.Value {
			return -1
		}
		return 1
	}
	if len(left.Prerelease) < len(right.Prerelease) {
		return -1
	}
	if len(left.Prerelease) > len(right.Prerelease) {
		return 1
	}
	return 0
}

func IsDevelopment(v Version) bool {
	for _, part := range v.Prerelease {
		if part.Value == "dev" {
			return true
		}
	}
	for _, part := range v.Metadata {
		if part == "dirty" {
			return true
		}
	}
	return false
}

func parseNumeric(input string) (uint64, error) {
	if !isNumeric(input) || (len(input) > 1 && input[0] == '0') {
		return 0, errors.New("version core identifiers must be unsigned integers without leading zeroes")
	}
	return strconv.ParseUint(input, 10, 64)
}

func parseTextIdentifiers(input string, prerelease bool) ([]string, error) {
	if input == "" {
		return nil, errors.New("version identifier is empty")
	}
	parts := strings.Split(input, ".")
	for _, part := range parts {
		if part == "" {
			return nil, errors.New("version identifier is empty")
		}
		for _, char := range part {
			if (char < '0' || char > '9') && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && char != '-' {
				return nil, errors.New("version identifier contains an invalid character")
			}
		}
		if prerelease && isNumeric(part) && len(part) > 1 && part[0] == '0' {
			return nil, errors.New("numeric prerelease identifier has a leading zero")
		}
	}
	return parts, nil
}

func isNumeric(input string) bool {
	if input == "" {
		return false
	}
	for _, char := range input {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
