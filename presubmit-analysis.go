package main

import (
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

type Presubmit struct {
	Name      string `yaml:"name"`
	AlwaysRun bool   `yaml:"always_run"`
	Optional  bool   `yaml:"optional"`
}

type Presubmits struct {
	PresubmitJobs map[string][]Presubmit `yaml:"presubmits"`
}

type Build struct {
	Result string `json:"Result"`
}

func main() {
	resp, err := http.Get("https://raw.githubusercontent.com/openshift/release/master/ci-operator/jobs/openshift/ovn-kubernetes/openshift-ovn-kubernetes-master-presubmits.yaml")
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	var presubmits Presubmits

	err = yaml.Unmarshal(data, &presubmits)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	var jobs []Presubmit
	for _, jobList := range presubmits.PresubmitJobs {
		for _, job := range jobList {
			jobs = append(jobs, job)
		}
	}

	for _, job := range jobs {
		// only care about e2e jobs that run on every PR
		if strings.Contains(job.Name, "e2e") && job.AlwaysRun != false {
			url := fmt.Sprintf("https://prow.ci.openshift.org/job-history/gs/origin-ci-test/pr-logs/directory/%s?buildId=", job.Name)
			successCount, failureCount, err := getJobHistory(url, 3)
			if err != nil {
				log.Fatalf("error: %v", err)
			}
			fmt.Printf("Job name: %s, AlwaysRun: %t, Optional: %t\n", job.Name, job.AlwaysRun, job.Optional)
			fmt.Printf("\t\tSUCCESS count: %d, FAILURE count: %d\n", successCount, failureCount)
		}
	}

	if err != nil {
		log.Fatalf("error: %v", err)
	}

}

func getJobHistory(url string, depth int) (int, int, error) {
	successCount := 0
	failureCount := 0

	err := processPage(url, &successCount, &failureCount, depth)
	if err != nil {
		return 0, 0, err
	}

	return successCount, failureCount, nil
}

func processPage(url string, successCount *int, failureCount *int, depth int) error {
	if depth >= 0 {
		resp, err := http.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		doc, err := goquery.NewDocumentFromReader(resp.Body)
		if err != nil {
			return err
		}

		var js string
		// Find the script tag with `allBuilds` variable
		doc.Find("script").Each(func(i int, s *goquery.Selection) {
			if strings.Contains(s.Text(), "var allBuilds") {
				js = s.Text()
			}
		})

		js = strings.TrimSpace(js)
		js = strings.TrimPrefix(js, "var allBuilds = ")
		js = strings.TrimSuffix(js, ";")

		// Unmarshal the JSON
		var builds []Build
		err = json.Unmarshal([]byte(js), &builds)
		if err != nil {
			return err
		}

		for _, build := range builds {
			if build.Result == "SUCCESS" {
				*successCount++
			} else if build.Result == "FAILURE" {
				*failureCount++
			}
		}

		// Find "Older Runs" link and process the page it points to
		doc.Find("a").Each(func(i int, s *goquery.Selection) {
			if s.Text() == "<- Older Runs" {
				olderRunsURL, exists := s.Attr("href")
				if exists {
					// Prepend the base URL, because the URL is relative
					olderRunsURL = "https://prow.ci.openshift.org" + olderRunsURL
					err = processPage(olderRunsURL, successCount, failureCount, depth-1)
				}
			}
		})
	}
	return nil
}
