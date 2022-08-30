// Copyright Amazon.com Inc or its affiliates and the project contributors
// Written by James Shubin <purple@amazon.com> and the project contributors
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.
//
// We will never require a CLA to submit a patch. All contributions follow the
// `inbound == outbound` rule.
//
// This is not an official Amazon product. Amazon does not offer support for
// this project.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/awslabs/yesiscan/interfaces"
	"github.com/awslabs/yesiscan/lib"
	"github.com/awslabs/yesiscan/s3"
	"github.com/awslabs/yesiscan/util/errwrap"
	"github.com/awslabs/yesiscan/web"

	"github.com/mitchellh/go-homedir"
	"github.com/ssgelm/cookiejarparser"
	cli "github.com/urfave/cli/v2" // imports as package "cli"
)

// Hide a program/version string for build embedding.
//go:generate bash -c "basename $(pwd) | tr -d '\n' > .program"
//go:generate bash -c "git describe --match '[0-9]*.[0-9]*.[0-9]*' --tags --dirty --always > .version"

//go:embed .program
var program string

//go:embed .version
var version string

// autoConfigURI is set via -ldflags build time flags.
var autoConfigURI string

// autoConfigCookiePath is set via -ldflags build time flags.
var autoConfigCookiePath string

const (
	// ConfigFileName is the name of the config file used to pull in all the
	// various main settings that we want.
	ConfigFileName = "config.json"

	// MaxRedirects is the maximum number of redirects to allow for http
	// download operations. The internal golang maximum of ten is too low
	// for many situations. Firefox sets network.http.redirection-limit as
	// 20.
	MaxRedirects = 20 // do what firefox does
)

// CLI is the entry point for the CLI frontend.
func CLI(program, version string, debug bool, logf func(format string, v ...interface{})) error {

	flags := []cli.Flag{
		&cli.StringFlag{Name: "auto-config-uri"},
		&cli.StringFlag{Name: "auto-config-cookie-path"},
		&cli.BoolFlag{Name: "quiet"},
		&cli.StringFlag{Name: "regexp-path"},
		&cli.StringFlag{Name: "config-path"},
		&cli.StringFlag{Name: "output-type"},
		&cli.StringFlag{Name: "output-path"},
		&cli.StringFlag{Name: "output-s3bucket"},
		&cli.StringFlag{Name: "region"},
		&cli.StringSliceFlag{Name: "profile"},
	}
	// build the yes and no backend flags
	for _, b := range lib.Backends {
		f := &cli.BoolFlag{Name: fmt.Sprintf("no-backend-%s", b)}
		flags = append(flags, f)
	}
	for _, b := range lib.Backends {
		f := &cli.BoolFlag{Name: fmt.Sprintf("yes-backend-%s", b)}
		flags = append(flags, f)
	}

	app := &cli.App{
		Name:  program,
		Usage: "scan code for legal things",
		Action: func(c *cli.Context) error {
			logf("Hello from purpleidea! This is %s, version: %s", program, version)
			defer logf("Done!")

			return App(c, program, version, debug, logf)
		},
		Flags:                flags,
		EnableBashCompletion: true,

		Commands: []*cli.Command{
			{
				Name:    "web",
				Aliases: []string{"web"},
				Usage:   "launch a web server mode",
				Action: func(c *cli.Context) error {
					logf("Hello from purpleidea! This is %s, version: %s", program, version)
					defer logf("Done!")

					return Web(c, program, version, debug, logf)
				},
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "profile"},
				},
			},
		},
	}

	return app.Run(os.Args)
}

// App is the main entry point action for the regular yesiscan cli application.
func App(c *cli.Context, program, version string, debug bool, logf func(format string, v ...interface{})) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	bigIntStr := "" // for our int
	var quiet bool
	var regexpPath string
	// config-path makes no sense here
	var outputType string
	var outputPath string
	var outputS3Bucket string
	region := s3.DefaultRegion
	profiles := []string{}
	backends := make(map[string]bool)

	// load from main config file or xdg if config is empty
	config, err := GetConfig(c.String("config-path"))
	if err != nil {
		return err
	}
	if config != nil {
		if config.AutoConfigURI != nil {
			// set this global var
			autoConfigURI = *config.AutoConfigURI
		}
		if config.AutoConfigCookiePath != nil {
			// set this global var
			autoConfigCookiePath = *config.AutoConfigCookiePath
		}
		if config.Quiet != nil {
			quiet = *config.Quiet
		}
		if config.RegexpPath != nil {
			regexpPath = *config.RegexpPath
		}
		// config-path makes no sense here
		if config.OutputType != nil {
			outputType = *config.OutputType
		}
		if config.OutputPath != nil {
			outputPath = *config.OutputPath
		}
		if config.OutputS3Bucket != nil {
			outputS3Bucket = *config.OutputS3Bucket
		}
		if config.Region != nil {
			region = *config.Region
		}
		if config.Profiles != nil {
			profiles = []string{} // erase any previous
			for _, x := range *config.Profiles {
				profiles = append(profiles, x)
			}
		}
		if config.Backends != nil {
			for k, v := range config.Backends {
				backends[k] = v // copy
			}
		}
	}

	// Command line options override anything in the config.
	if c.IsSet("auto-config-uri") {
		autoConfigURI = c.String("auto-config-uri")
	}
	if c.IsSet("auto-config-cookie-path") {
		autoConfigCookiePath = c.String("auto-config-cookie-path")
	}
	if c.IsSet("quiet") {
		quiet = c.Bool("quiet")
	}
	if c.IsSet("regexp-path") {
		regexpPath = c.String("regexp-path")
	}
	// config-path makes no sense here
	if c.IsSet("output-type") {
		outputType = c.String("output-type")
	}
	if c.IsSet("output-path") {
		outputPath = c.String("output-path")
	}
	if c.IsSet("output-s3bucket") {
		outputS3Bucket = c.String("output-s3bucket")
	}
	if c.IsSet("region") {
		region = c.String("region")
	}
	if c.IsSet("profile") {
		profiles = []string{} // erase any previous
		for _, x := range c.StringSlice("profile") {
			profiles = append(profiles, x)
		}
	}

	// auto config URI magic...
	if autoConfigURI != "" { // we must try to auto config
		logf("getting config from: %s", autoConfigURI)
		data, err := DownloadConfig(autoConfigURI)
		if err != nil {
			return errwrap.Wrapf(err, "autoConfigURI download failed on: %s", autoConfigURI)
		}
		p, err := GetConfigPath(c.String("config-path"))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) { // no config exists here...
			b = []byte{} // (implied, but now cleanly initialized)
			err = nil    // clear this
		} else if err != nil {
			return err
		}

		isJson := func(d []byte) error {
			buffer := bytes.NewBuffer(d)
			if buffer.Len() == 0 {
				return fmt.Errorf("empty config file")
			}
			decoder := json.NewDecoder(buffer)

			var configData Config // this gets populated during decode
			err := decoder.Decode(&configData)
			return errwrap.Wrapf(err, "invalid json")
		}

		// if equal, we don't need to change the config...
		// check it's valid json before writing it? (for portal errors)
		if err := isJson(data); !bytes.Equal(data, b) && err == nil {

			// store new config file
			logf("writing new config...")
			// XXX: set umask for u=rw,go=
			if err := os.WriteFile(p, data, interfaces.Umask); err != nil {
				return errwrap.Wrapf(err, "autoConfigURI store failed on: %s", p)
			}

			// recurse!
			logf("recursing on new config...")
			return App(c, program, version, debug, logf)

		} else if err != nil {
			// provide logs so users know something is wrong...
			logf("invalid config file at URI: %s", autoConfigURI)
			logf("error with config file: %+v", err)
			if autoConfigCookiePath == "" {
				logf("do you need an auth cookie?")
			} else {
				logf("is your auth cookie (%s) valid?", autoConfigCookiePath)
			}
		}
	}

	// XXX: pull from additional config file download paths and destinations

	if outputPath == "-" || quiet { // if output is stdout, noop logs
		logf = func(format string, v ...interface{}) {
			// noop
		}
	}
	args := []string{}
	for i := 0; i < c.NArg(); i++ {
		s := c.Args().Get(i)
		args = append(args, s)
	}

	// is there at least one yes-?
	isAdditive := false
	for _, f := range lib.Backends {
		if c.Bool(fmt.Sprintf("yes-backend-%s", f)) {
			isAdditive = true
		}
	}

	// isBackendEnabled specifies if a particular backend
	// should be enabled based on the lookup of the flags.
	isBackendEnabled := func(f string) bool {
		if isAdditive && c.Bool(fmt.Sprintf("yes-backend-%s", f)) {
			return true
		}

		if !isAdditive && !c.Bool(fmt.Sprintf("no-backend-%s", f)) {
			return true
		}

		return false
	}

	for _, b := range lib.Backends {
		// if undefined, then look at the flags...
		if _, exists := backends[b]; !exists {
			backends[b] = isBackendEnabled(b)
			continue
		}
		// if the yes or no flag was set, then use that
		if c.Bool(fmt.Sprintf("no-backend-%s", b)) {
			backends[b] = false
		}
		if c.Bool(fmt.Sprintf("yes-backend-%s", b)) {
			backends[b] = true
		}
	}

	if outputS3Bucket != "" { // do a test-for-auth run

		bigInt, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
		if err != nil {
			return errwrap.Wrapf(err, "random number generation error")
		}
		bigIntStr = bigInt.String()

		objectName := program // arbitrary, but unique
		contentType := "text/plain"
		inputs := &s3.Inputs{
			Region:            region,
			BucketName:        outputS3Bucket,
			CreateBucket:      true,
			ObjectName:        objectName,
			GrantReadAllUsers: true,
			ContentType:       &contentType,
			Data:              []byte(program), // arbitrary
			Debug:             debug,
			Logf: func(format string, v ...interface{}) {
				logf("s3: "+format, v...)
			},
		}
		// XXX: find a way to check if credentials are
		// good, early in the operation before the scan,
		// otherwise we will end up running a whole scan
		// and then throwing away the results...
		logf("s3: setup verification...")
		if _, err := s3.Store(ctx, inputs); err != nil {
			return errwrap.Wrapf(err, "s3 setup error")
		}
	}

	m := &lib.Main{
		Program: program,
		Version: version,
		Debug:   debug,
		Logf:    logf,

		Args:     args,
		Backends: backends,

		Profiles: profiles,

		RegexpPath: regexpPath,
	}

	output, err := m.Run(ctx)
	if err != nil {
		return err
	}

	s := ""
	if outputPath != "" || outputS3Bucket != "" {
		var err error
		// TODO: when we render an html version, should
		// it look the same as the web `save` output?
		if outputType == "text" {
			if s, err = lib.ReturnOutputFile(output); err != nil {
				return err
			}
		} else {
			if s, err = web.ReturnOutputHtml(output); err != nil {
				return err
			}
		}
	}

	if outputS3Bucket != "" {
		ext := "html"
		contentType := "text/html"
		if outputType == "text" {
			ext = "txt"
			contentType = "text/plain"
		}

		// make a unique ID for the file
		// XXX: we can consider different algorithms or methods here later...
		// We want this hash to be basically impossible
		// to guess, so that you can only get it if you
		// have the secret link.
		if bigIntStr == "" { // make sure we really have one
			// programming error
			return fmt.Errorf("random number generation logic error")
		}
		now := strconv.FormatInt(time.Now().UnixMilli(), 10) // itoa but int64
		sum := sha512.Sum512([]byte(s + now + bigIntStr))    // XXX: for now
		uid := fmt.Sprintf("%x", sum)
		objectName := fmt.Sprintf("%s-%s.%s", program, uid, ext) // TODO: arbitrary

		inputs := &s3.Inputs{
			Region:            region,
			BucketName:        outputS3Bucket,
			CreateBucket:      true,
			ObjectName:        objectName,
			GrantReadAllUsers: true,
			ContentType:       &contentType,
			Data:              []byte(s),
			Debug:             debug,
			Logf: func(format string, v ...interface{}) {
				logf("s3: "+format, v...)
			},
		}
		// XXX: find a way to check if credentials are
		// good, early in the operation before the scan,
		// otherwise we will end up running a whole scan
		// and then throwing away the results...
		u, err := s3.Store(ctx, inputs)
		if err != nil {
			logf("could not write s3 file: %+v", err)
		} else {
			fmt.Printf("S3 Sig URL: %s\n", u)
			fmt.Printf("S3 Pub URL: %s\n", s3.PubURL(region, outputS3Bucket, objectName))
		}
	}

	if outputPath == "-" {
		// NOTE: if we get asked for stdout, we
		// turn off other output to make it sane
		// TODO: should logs go to stderr instead?
		quiet = true           // redundant for now
		_, err := fmt.Print(s) // to stdout
		return err

	} else if outputPath != "" {
		// TODO: is this the umask we should use?
		if err := os.WriteFile(outputPath, []byte(s), interfaces.Umask); err != nil {
			logf("could not write output file: %+v", err)
		}
	}

	if !quiet {
		s, err := lib.ReturnOutputConsole(output)
		if err != nil {
			return err
		}

		fmt.Print(s) // display it
	}

	return nil
}

// Config is a list of settings stored in the users ~/.config/ directory.
// TODO: should this get moved into the lib package?
type Config struct {
	// AutoConfigURI is a special URI which if set, will try and pull a
	// config from that location on startup. It will use the cookie file
	// stored at AutoConfigCookiePath if specified. If successful, it will
	// check if the config is different from what is currently stored. If so
	// then it will validate if it is a valid json config. If so it will
	// replace (overwrite!) the current config and then recursively begin
	// the process again. The only thing preventing infinite recursion here
	// is the fact that you probably would not chain 100 configs, one after
	// another...
	AutoConfigURI *string `json:"auto-config-uri"`

	// AutoConfigCookiePath is a special URI which if set will point to a
	// netscape/libcurl style cookie file to use when making the get
	// download requests. This is useful if you store your config behind
	// some gateway that needs a magic cookie for auth.
	AutoConfigCookiePath *string `json:"auto-config-cookie-path"`

	// Quiet will prevent the tool from talking too much on the console.
	// This is implied if you use the stdout option of --output-path.
	Quiet *bool `json:"quiet"`

	// RegexpPath specifies a path the regular expressions to use.
	RegexpPath *string `json:"regexp-path"`
	// config-path makes no sense here

	// OutputType is the format the report will be sent as. Options include
	// "html" and "text".
	OutputType *string `json:"output-type"`

	// OutputPath is the location where the report will be saved. This will
	// overwrite any existing file at this location. Use with caution. If
	// you specify the - character (dash) then it will print to stdout.
	OutputPath *string `json:"output-path"`

	// OutputS3Bucket prints the report to an S3 bucket with this name. Make
	// sure you don't have anything important in the bucket as it might
	// overwrite any file in there as the report name is chosen
	// automatically.
	OutputS3Bucket *string `json:"output-s3bucket"`

	// Region specifies the S3 region to use when writing to the S3 bucket.
	Region *string `json:"region"`

	// Profiles is the list of profiles to use. Either the names from
	// ~/.config/yesiscan/profiles/<name>.json or full paths.
	Profiles *[]string `json:"profiles"`

	// Backends gives us a list of backends we use. If the corresponding
	// bool value in the map is true, then the backend is enabled. If it is
	// false that it is not enabled. If it not listed then its behaviour is
	// undefined.
	Backends map[string]bool `json:"backends"`
}

// GetConfig loads the config file data into a struct.
func GetConfig(p string) (*Config, error) {

	configPath, err := GetConfigPath(p)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) && p == "" {
		return nil, nil // no config, no error
	}
	if err != nil {
		return nil, errwrap.Wrapf(err, "error reading config file")
	}

	buffer := bytes.NewBuffer(data)
	if buffer.Len() == 0 {
		return nil, fmt.Errorf("empty config file: %s", configPath)
	}
	decoder := json.NewDecoder(buffer)

	var configData Config // this gets populated during decode
	if err := decoder.Decode(&configData); err != nil {
		// TODO: should this be an error, or just a silent ignore?
		return nil, errwrap.Wrapf(err, "error decoding json output of: %s", configPath)
	}

	return &configData, nil
}

// GetConfigPath returns the expected path to the main config.json file given
// the input arg for that setting.
func GetConfigPath(configPath string) (string, error) {
	// If config path is set, we look in there for a config, otherwise we
	// use the default xdg path.
	if configPath != "" {
		return configPath, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", errwrap.Wrapf(err, "error finding home directory")
	}
	if home == "" {
		return "", fmt.Errorf("home directory is empty")
	}

	// TODO: get program from an input var perhaps?
	p := filepath.Join(home, ".config/", program+"/", ConfigFileName)
	return filepath.Clean(p), nil
}

// DownloadConfig pulls a config from a magic URI and returns the contents.
func DownloadConfig(uri string) ([]byte, error) {
	if uri == "" {
		return nil, fmt.Errorf("empty URI")
	}

	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	if u.Scheme == "https" {
		client := &http.Client{
			CheckRedirect: func() func(req *http.Request, via []*http.Request) error {
				redirects := 0
				return func(req *http.Request, via []*http.Request) error {
					if redirects > MaxRedirects {
						return fmt.Errorf("stopped after %d redirects", MaxRedirects)
					}
					redirects++
					return nil
				}
			}(),
		}
		if autoConfigCookiePath != "" {
			p, err := homedir.Expand(autoConfigCookiePath)
			if err != nil {
				return nil, errwrap.Wrapf(err, "invalid path of: %s", autoConfigCookiePath)
			}
			cookieJar, err := cookiejarparser.LoadCookieJarFile(p)
			if err != nil {
				return nil, errwrap.Wrapf(err, "error loading cookie from: %s", autoConfigCookiePath)
			}
			client.Jar = cookieJar
		}

		resp, err := client.Get(uri)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return body, nil
	}

	return nil, fmt.Errorf("unsupported URI: %s", uri)
}

func main() {
	debug := false // TODO: hardcoded for now
	logf := func(format string, v ...interface{}) {
		fmt.Fprintf(os.Stderr, "main: "+format+"\n", v...)
	}
	program = strings.TrimSpace(program)
	version = strings.TrimSpace(version)
	if program == "" || version == "" {
		// run `go generate` before you build it.
		logf("program was not compiled correctly")
		os.Exit(1)
		return
	}

	// FIXME: We discard output from lib's that use `log` package directly.
	log.SetOutput(io.Discard)

	err := CLI(program, version, debug, logf) // TODO: put these args in an input struct
	if err != nil {
		if debug {
			logf("failed: %+v", err)
		} else {
			logf("failed: %+v", errwrap.Cause(err))
		}
		os.Exit(1)
		return
	}
	os.Exit(0)
}
