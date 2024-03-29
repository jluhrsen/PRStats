package main

import (
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

type Presubmit struct {
	Name          string `yaml:"name"`
	AlwaysRun     bool   `yaml:"always_run"`
	Optional      bool   `yaml:"optional"`
	SuccessCount  int
	FailureCount  int
	AbortedCount  int
	PendingCount  int
	ErrorCount    int
	UnknownCount  int
	PassRate      float64
	TotalJobCount int
}

type Presubmits struct {
	PresubmitJobs map[string][]Presubmit `yaml:"presubmits"`
}

type Build struct {
	Result string `json:"Result"`
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Please provide the project name for presubmit analysis.")
	}

	project := os.Args[1]

	url := fmt.Sprintf("https://raw.githubusercontent.com/openshift/release/master/ci-operator/jobs/openshift/%s/openshift-%s-master-presubmits.yaml", project, project)
	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
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
			// only care about e2e jobs that run on every PR
			if strings.Contains(job.Name, "e2e") && job.AlwaysRun != false {
				jobs = append(jobs, job)
			}
		}
	}

	// how many pages of results to look at (20 per page)
	resultsDepth := 2

	for i, job := range jobs {
		url := fmt.Sprintf("https://prow.ci.openshift.org/job-history/gs/origin-ci-test/pr-logs/directory/%s?buildId=", job.Name)
		successCount, failureCount, abortedCount, pendingCount, errorCount, unexpectedStatusCount, unknownCount, err := getJobHistory(url, resultsDepth)
		if err != nil {
			log.Fatalf("error: %v", err)
		}

		totalJobCount := successCount + failureCount + abortedCount + pendingCount + errorCount + unknownCount
		if unexpectedStatusCount > 0 {
			log.Fatalf("Did not parse proper number of expected jobs for %s.\nExpected %d, but got %d unexpected statuses", url, (resultsDepth+1)*20, unexpectedStatusCount)
		}

		passRate := 0.0
		if totalJobCount != 0 { // to avoid division by zero
			passRate = float64(successCount) / (float64(successCount) + float64(failureCount))
		}

		jobs[i].SuccessCount = successCount
		jobs[i].FailureCount = failureCount
		jobs[i].AbortedCount = abortedCount
		jobs[i].PendingCount = pendingCount
		jobs[i].ErrorCount = errorCount
		jobs[i].UnknownCount = unknownCount
		jobs[i].PassRate = passRate
		jobs[i].TotalJobCount = totalJobCount

		fmt.Printf("Job name: %s, AlwaysRun: %t, Optional: %t\n", job.Name, job.AlwaysRun, job.Optional)
		fmt.Printf("\t\tSUCCESS count: %d, FAILURE count: %d, ABORTED count: %d, PENDING count: %d, ERROR count: %d, UNKNOWN count: %d\n",
			successCount, failureCount, abortedCount, pendingCount, errorCount, unknownCount)
		fmt.Printf("\t\t\tPASS RATE: %.0f%%\n", passRate*100)
	}

	jsonData, err := json.Marshal(jobs)
	if err != nil {
		log.Fatalf("Failed to marshal jobs to JSON: %v", err)
	}

	// Write JSON data to a file
	file, err := os.Create("presubmit_jobs.json")
	if err != nil {
		log.Fatalf("Failed to create file: %v", err)
	}
	defer file.Close()

	_, err = file.Write(jsonData)
	if err != nil {
		log.Fatalf("Failed to write JSON data to file: %v", err)
	}

}

func getJobHistory(url string, depth int) (int, int, int, int, int, int, int, error) {
	successCount := 0
	failureCount := 0
	abortedCount := 0
	pendingCount := 0
	errorCount := 0
	unknownCount := 0
	unexpectedStatusCount := 0

	err := processPage(url, &successCount, &failureCount, &abortedCount, &pendingCount, &errorCount, &unexpectedStatusCount, &unknownCount, depth)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, 0, err
	}

	return successCount, failureCount, abortedCount, pendingCount, errorCount, unexpectedStatusCount, unknownCount, nil
}

func processPage(url string, successCount *int, failureCount *int, abortedCount *int, pendingCount *int, errorCount *int, unexpectedStatusCount *int, unknownCount *int, depth int) error {
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
			} else if build.Result == "ABORTED" {
				*abortedCount++
			} else if build.Result == "PENDING" {
				*pendingCount++
			} else if build.Result == "ERROR" {
				*errorCount++
			} else if build.Result == "UNKNOWN" {
				*unknownCount++
			} else {
				*unexpectedStatusCount++
			}
		}

		// Find "Older Runs" link and process the page it points to
		doc.Find("a").Each(func(i int, s *goquery.Selection) {
			if s.Text() == "<- Older Runs" {
				olderRunsURL, exists := s.Attr("href")
				if exists {
					// Prepend the base URL, because the URL is relative
					olderRunsURL = "https://prow.ci.openshift.org" + olderRunsURL
					err = processPage(olderRunsURL, successCount, failureCount, abortedCount, pendingCount, errorCount, unexpectedStatusCount, unknownCount, depth-1)
				}
			}
		})
	}
	return nil
}
