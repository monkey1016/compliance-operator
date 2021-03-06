/*
Copyright © 2020 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/subchen/go-xmldom"

	"github.com/ghodss/yaml"

	"k8s.io/client-go/kubernetes"

	"github.com/openshift/compliance-operator/pkg/utils"
)

const (
	contentFileTimeout = 3600
)

// For OpenSCAP content as an XML data stream. Implements ResourceFetcher.
type scapContentDataStream struct {
	// Client for Gets
	client *kubernetes.Clientset
	// Staging objects
	dataStream *utils.XMLDocument
	resources  []string
	found      map[string][]byte
}

func (c *scapContentDataStream) LoadSource(path string) error {
	f, err := openNonEmptyFile(path)
	if err != nil {
		return err
	}
	// #nosec
	defer f.Close()
	xml, err := parseContent(f)
	if err != nil {
		return err
	}
	c.dataStream = xml
	return nil
}

func parseContent(f *os.File) (*utils.XMLDocument, error) {
	return utils.ParseContent(bufio.NewReader(f))
}

// Returns the file, but only after it has been created by the other init container.
// This avoids a race.
func openNonEmptyFile(filename string) (*os.File, error) {
	readFileTimeoutChan := make(chan *os.File, 1)

	// gosec complains that the file is passed through an evironment variable. But
	// this is not a security issue because none of the files are user-provided
	cleanFileName := filepath.Clean(filename)

	go func() {
		for {
			// Note that we're cleaning the filename path above.
			// #nosec
			file, err := os.Open(cleanFileName)
			if err == nil {
				fileinfo, err := file.Stat()
				// Only try to use the file if it already has contents.
				if err == nil && fileinfo.Size() > 0 {
					readFileTimeoutChan <- file
				}
			} else if !os.IsNotExist(err) {
				fmt.Println(err)
				os.Exit(1)
			}
			time.Sleep(1 * time.Second)
		}
	}()

	select {
	case file := <-readFileTimeoutChan:
		fmt.Printf("File '%s' found, using.\n", filename)
		return file, nil
	case <-time.After(time.Duration(contentFileTimeout) * time.Second):
		fmt.Println("Timeout. Aborting.")
		os.Exit(1)
	}

	// We shouldn't get here.
	return nil, nil
}

func (c *scapContentDataStream) FigureResources(profile string) error {
	found := getResourcePaths(c.dataStream, profile)
	if len(found) == 0 {
		return fmt.Errorf("no checks found in datastream")
	}
	// Always stage the clusteroperators/openshift-apiserver object for version detection.
	paths := []string{"/apis/config.openshift.io/v1/clusteroperators/openshift-apiserver"}
	paths = append(paths, found...)

	c.resources = paths
	return nil
}

const (
	endPointTag    = "ocp-api-endpoint"
	endPointTagEnd = endPointTag + "\">"
	codeTag        = "</code>"
)

// getPathsFromRuleWarning finds the API endpoint from in. The expected structure is:
//
//  <warning category="general" lang="en-US"><code class="ocp-api-endpoint">/apis/config.openshift.io/v1/oauths/cluster
//  </code></warning>
func getPathFromWarningXML(in string) string {
	DBG("%s", in)
	apiIndex := strings.Index(in, endPointTag)
	if apiIndex == -1 {
		return ""
	}

	apiValueBeginIndex := apiIndex + len(endPointTagEnd)
	apiValueEndIndex := strings.Index(in[apiValueBeginIndex:], codeTag)
	if apiValueEndIndex == -1 {
		return ""
	}

	return in[apiValueBeginIndex : apiValueBeginIndex+apiValueEndIndex]
}

// Collect the resource paths for objects that this scan needs to obtain.
// The profile will have a series of "selected" checks that we grab all of the path info from.
func getResourcePaths(ds *utils.XMLDocument, profile string) []string {
	out := []string{}
	selectedChecks := []string{}

	// First we find the Profile node, to locate the enabled checks.
	DBG("Using profile %s", profile)
	nodes := ds.Root.Query("//Profile")
	for _, node := range nodes {
		profileID := node.GetAttributeValue("id")
		if profileID != profile {
			continue
		}

		checks := node.GetChildren("select")
		for _, check := range checks {
			if check.GetAttributeValue("selected") != "true" {
				continue
			}

			if idRef := check.GetAttributeValue("idref"); idRef != "" {
				DBG("selected: %v", idRef)
				selectedChecks = append(selectedChecks, idRef)
			}
		}
	}

	checkDefinitions := ds.Root.Query("//Rule")
	if len(checkDefinitions) == 0 {
		DBG("WARNING: No rules to query (invalid datastream)")
		return out
	}

	// For each of our selected checks, collect the required path info.
	for _, checkID := range selectedChecks {
		var found *xmldom.Node
		for _, rule := range checkDefinitions {
			if rule.GetAttributeValue("id") == checkID {
				found = rule
				break
			}
		}
		if found == nil {
			DBG("WARNING: Couldn't find a check for id %s", checkID)
			continue
		}

		// This node is called "warning" and contains the path info. It's not an actual "warning" for us here.
		warning := found.GetChild("warning")
		if warning == nil {
			DBG("Couldn't find 'warning' child of check %s", checkID)
			continue
		}

		apiPath := getPathFromWarningXML(warning.XML())
		if len(apiPath) == 0 {
			continue
		}
		out = append(out, apiPath)
	}

	return out
}

func (c *scapContentDataStream) FetchResources() error {
	found, err := fetch(c.client, c.resources)
	if err != nil {
		return err
	}
	c.found = found
	return nil
}

func fetch(client *kubernetes.Clientset, objects []string) (map[string][]byte, error) {
	results := map[string][]byte{}
	for _, uri := range objects {
		err := func() error {
			LOG("Fetching URI: '%s'", uri)
			req := client.RESTClient().Get().RequestURI(uri)
			stream, err := req.Stream(context.TODO())
			if err != nil {
				return err
			}
			defer stream.Close()
			body, err := ioutil.ReadAll(stream)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				DBG("no data in request body")
				return nil
			}
			yaml, err := yaml.JSONToYAML(body)
			if err != nil {
				return err
			}
			results[uri] = yaml
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (c *scapContentDataStream) SaveResources(to string) error {
	return saveResources(to, c.found)
}

func saveResources(rootDir string, data map[string][]byte) error {
	for apiPath, fileContents := range data {
		saveDir, saveFile, err := getSaveDirectoryAndFileName(rootDir, apiPath)
		savePath := path.Join(saveDir, saveFile)
		LOG("Saving fetched resource to: '%s'", savePath)
		if err != nil {
			return err
		}
		err = os.MkdirAll(saveDir, 0700)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(savePath, fileContents, 0600)
		if err != nil {
			return err
		}
	}
	return nil
}

// Returns the absolute directory path (including rootDir) and filename for the given apiPath.
func getSaveDirectoryAndFileName(rootDir string, apiPath string) (string, string, error) {
	base := path.Base(apiPath)
	if base == "." || base == "/" {
		return "", "", fmt.Errorf("bad object path: %s", apiPath)
	}
	subDirs := path.Dir(apiPath)
	if subDirs == "." {
		return "", "", fmt.Errorf("bad object path: %s", apiPath)
	}

	return path.Join(rootDir, subDirs), base, nil
}
