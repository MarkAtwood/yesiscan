// Package licenses provides some structures for handling and representing
// software licenses. It uses SPDX representations for part of it, because there
// doesn't seem to be a better alternative. It doesn't guarantee that it
// implements all of the SPDX spec. If there's an aspect which you think was
// mis-implemented or is missing, please let us know.
// XXX: Add a test to check if the license-list-data submodule is up-to-date!
package licenses

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// licensesJson is populated automatically at build-time from the official spdx
// licenses.json file, which is linked into this repository as a git submodule.
//go:embed license-list-data/json/licenses.json
var licensesJSON []byte

//go:embed license-list-data/json/details/*.json
var licensesTextJSON embed.FS

//go:embed license-list-data/json/exceptions.json
var exceptionsJson []byte

//go:embed license-list-data/json/exceptions/*.json
var exceptionsTextJSON embed.FS

var (
	once        sync.Once
	LicenseList LicenseListSPDX // this gets populated during init()
)

func init() {
	once.Do(decode)
}

// TODO: import the exceptions if we ever decide we want to look at those.
func decode() {
	buffer := bytes.NewBuffer(licensesJSON)
	decoder := json.NewDecoder(buffer)
	if err := decoder.Decode(&LicenseList); err != nil {
		panic(fmt.Sprintf("error decoding spdx license list: %+v", err))
	}
	if len(LicenseList.Licenses) == 0 {
		panic(fmt.Sprintf("could not find any licenses to decode"))
	}

	// debug
	//dirEntry, err := licensesTextJSON.ReadDir("license-list-data/json/details")
	//if err != nil {
	//	panic(fmt.Sprintf("error: %+v", err))
	//}
	//for _, x := range dirEntry {
	//	fmt.Printf("Name: %+v\n", x.Name())
	//}

	for _, license := range LicenseList.Licenses {
		//fmt.Printf("ID: %+v\n", license.LicenseID) // debug

		f := "license-list-data/json/details/" + strings.TrimPrefix(license.Reference, "./")
		data, err := licensesTextJSON.ReadFile(f)
		if err != nil {
			panic(fmt.Sprintf("error reading spdx license file: %s, error: %+v", f, err))
		}
		//fmt.Printf("Data: %s\n", string(data)) // debug
		buffer := bytes.NewBuffer(data)
		decoder := json.NewDecoder(buffer)

		if err := decoder.Decode(&license); err != nil {
			panic(fmt.Sprintf("error decoding spdx license text: %+v", err))
		}
		//fmt.Printf("Text: %+v\n", license.Text) // debug
		if license.Text == "" {
			panic(fmt.Sprintf("could not find any license text for: %s", license.LicenseID))
		}
	}
}

// LicenseListSPDX is modelled after the official SPDX licenses.json file.
type LicenseListSPDX struct {
	Version string `json:"licenseListVersion"`

	Licenses []*LicenseSPDX `json:"licenses"`
}

// LicenseSPDX is modelled after the official SPDX license entries. It also
// includes fields from the referenced fields, which include the full text.
type LicenseSPDX struct {
	// Reference is a link to the full license .json file.
	Reference    string `json:"reference"`
	IsDeprecated bool   `json:"isDeprecatedLicenseId"`
	DetailsURL   string `json:"detailsUrl"`
	// ReferenceNumber is an index number for the license. I wouldn't
	// consider this to be stable over time.
	ReferenceNumber int64 `json:"referenceNumber"`
	// Name is a friendly name for the license.
	Name string `json:"name"`
	// LicenseID is the SPDX ID for the license.
	LicenseID     string   `json:"licenseId"`
	SeeAlso       []string `json:"seeAlso"`
	IsOSIApproved bool     `json:"isOsiApproved"`

	//IsDeprecated bool `json:"isDeprecatedLicenseId"` // appears again
	IsFSFLibre bool   `json:"isFsfLibre"`
	Text       string `json:"licenseText"`
}

// License is a representation of a license. It's better than a simple SPDX ID
// as a string, because it allows us to store alternative representations to an
// internal or different representation, as well as any other information that
// we want to have associated here.
type License struct {
	// SPDX is the well-known SPDX ID for the license.
	SPDX string

	// Origin shows a different license provenance, and associated custom
	// name. It should probably be a "reverse-dns" style unique identifier.
	Origin string
	// Custom is a custom string that is a unique identifier for the license
	// in the aforementioned Origin namespace.
	Custom string
}

// String returns a string representation of whatever license is specified.
func (obj *License) String() string {
	if obj.Origin != "" && obj.Custom != "" {
		return fmt.Sprintf("%s(%s)", obj.Custom, obj.Origin)
	}

	if obj.Origin == "" && obj.Custom != "" {
		return fmt.Sprintf("%s(unknown)", obj.Custom) // TODO: display this differently?
	}

	// TODO: replace with a different short name if one exists
	return obj.SPDX
}

// Validate returns an error if the license doesn't have a valid representation.
// For example, if you express the license as an SPDX ID, this will validate
// that it is among the known licenses.
func (obj *License) Validate() error {
	if obj.SPDX != "" {
		// if an SPDX ID is specified, we validate based on it!
		_, err := ID(obj.SPDX)
		return err
	}

	// valid, but from an unknown origin
	if obj.Origin != "" && obj.Custom != "" {
		return nil
	}

	if obj.Origin == "" && obj.Custom != "" {
		return fmt.Errorf("unknown custom license: %s", obj.Custom)
	}

	return fmt.Errorf("unknown license format")
}

// Cmp compares two licenses and determines if they are identical.
func (obj *License) Cmp(license *License) error {
	if obj.SPDX != license.SPDX {
		return fmt.Errorf("the SPDX field differs")
	}
	if obj.Origin != license.Origin {
		return fmt.Errorf("the Origin field differs")
	}
	if obj.Custom != license.Custom {
		return fmt.Errorf("the Custom field differs")
	}

	return nil
}

// ID looks up the license from the imported list. Do not modify the result as
// it is the global database that everyone is using.
func ID(spdx string) (*LicenseSPDX, error) {
	for _, license := range LicenseList.Licenses {
		if spdx == license.LicenseID {
			return license, nil
		}
	}
	return nil, fmt.Errorf("license ID (%s) not found", spdx)
}

// Join joins the string representations of a list of licenses with comma space.
func Join(licenses []*License) string {
	xs := []string{}
	for _, license := range licenses {
		xs = append(xs, license.String())
	}
	return strings.Join(xs, ", ")
}
