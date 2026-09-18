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

	data, branch, err := getPresubmitConfig(project)
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	log.Printf("using %q branch config for %s", branch, project)

	var presubmits Presubmits

	err = yaml.Unmarshal(data, &presubmits)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	var jobs []Presubmit
	for _, jobList := range presubmits.PresubmitJobs {
		for _, job := range jobList {
			// only care about e2e jobs. required vs optional is decided by the
			// optional field, not always_run: nearly every e2e presubmit is
			// always_run: false now and is triggered on demand instead.
			if strings.Contains(job.Name, "e2e") {
				jobs = append(jobs, job)
			}
		}
	}
	if len(jobs) == 0 {
		log.Fatalf("no e2e presubmit jobs found for %s on branch %s", project, branch)
	}

	// how many pages of results to look at (20 per page)
	resultsDepth := 2

	var results []Presubmit
	for _, job := range jobs {
		url := fmt.Sprintf("https://prow.ci.openshift.org/job-history/gs/origin-ci-test/pr-logs/directory/%s?buildId=", job.Name)
		successCount, failureCount, abortedCount, pendingCount, errorCount, unexpectedStatusCount, unknownCount, err := getJobHistory(url, resultsDepth)
		if err != nil {
			log.Fatalf("error: %v", err)
		}

		totalJobCount := successCount + failureCount + abortedCount + pendingCount + errorCount + unknownCount
		if unexpectedStatusCount > 0 {
			log.Printf("warning: %d unrecognized build statuses for %s", unexpectedStatusCount, job.Name)
		}
		if totalJobCount == 0 {
			// job is configured but has no recent runs, so it has nothing to chart
			log.Printf("skipping %s: no build history", job.Name)
			continue
		}

		passRate := 0.0
		if successCount+failureCount > 0 { // to avoid division by zero
			passRate = float64(successCount) / float64(successCount+failureCount)
		}

		job.SuccessCount = successCount
		job.FailureCount = failureCount
		job.AbortedCount = abortedCount
		job.PendingCount = pendingCount
		job.ErrorCount = errorCount
		job.UnknownCount = unknownCount
		job.PassRate = passRate
		job.TotalJobCount = totalJobCount
		results = append(results, job)

		fmt.Printf("Job name: %s, AlwaysRun: %t, Optional: %t\n", job.Name, job.AlwaysRun, job.Optional)
		fmt.Printf("\t\tSUCCESS count: %d, FAILURE count: %d, ABORTED count: %d, PENDING count: %d, ERROR count: %d, UNKNOWN count: %d\n",
			successCount, failureCount, abortedCount, pendingCount, errorCount, unknownCount)
		fmt.Printf("\t\t\tPASS RATE: %.0f%%\n", passRate*100)
	}

	if len(results) == 0 {
		log.Fatalf("no e2e presubmit jobs with build history found for %s", project)
	}

	jsonData, err := json.Marshal(results)
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

// getPresubmitConfig fetches the prow presubmit config for a project. The
// filename embeds the repo's default branch, which is not the same everywhere
// (ovn-kubernetes moved from master to main, cluster-network-operator did not),
// so try main first and fall back to master.
func getPresubmitConfig(project string) ([]byte, string, error) {
	for _, branch := range []string{"main", "master"} {
		url := fmt.Sprintf("https://raw.githubusercontent.com/openshift/release/master/ci-operator/jobs/openshift/%s/openshift-%s-%s-presubmits.yaml", project, project, branch)
		resp, err := http.Get(url)
		if err != nil {
			return nil, "", err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, "", err
		}
		switch resp.StatusCode {
		case http.StatusOK:
			return body, branch, nil
		case http.StatusNotFound:
			continue
		default:
			return nil, "", fmt.Errorf("GET %s returned %s", url, resp.Status)
		}
	}
	return nil, "", fmt.Errorf("no presubmit config for %s on branch main or master", project)
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

		if js == "" {
			// job has never run, or prow has no history page for it
			log.Printf("warning: no build history at %s", url)
			return nil
		}

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
