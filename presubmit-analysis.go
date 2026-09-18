package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v2"
)

type Presubmit struct {
	Name              string            `yaml:"name"`
	AlwaysRun         bool              `yaml:"always_run"`
	Optional          bool              `yaml:"optional"`
	RunIfChanged      string            `yaml:"run_if_changed" json:"-"`
	SkipIfOnlyChanged string            `yaml:"skip_if_only_changed" json:"-"`
	Annotations       map[string]string `yaml:"annotations" json:"-"`
	Stage             string
	SuccessCount      int
	FailureCount      int
	AbortedCount      int
	PendingCount      int
	ErrorCount        int
	UnknownCount      int
	PassRate          float64
	TotalJobCount     int
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

	// Each job costs one prow request per page of history, which walking the
	// list one job at a time turned into a twenty minute run.
	const workers = 8

	analyzed := make([]jobResult, len(jobs))
	queue := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				analyzed[i] = analyzeJob(jobs[i], resultsDepth)
			}
		}()
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	wg.Wait()

	// Report in the order the jobs were configured rather than the order the
	// workers happened to finish in.
	var results []Presubmit
	for _, analysis := range analyzed {
		if analysis.err != nil {
			log.Fatalf("error: %v", analysis.err)
		}
		for _, note := range analysis.notes {
			log.Print(note)
		}
		if !analysis.charted {
			continue
		}
		job := analysis.job
		results = append(results, job)

		fmt.Printf("Job name: %s, Stage: %s, Optional: %t\n", job.Name, job.Stage, job.Optional)
		fmt.Printf("\t\tSUCCESS count: %d, FAILURE count: %d, ABORTED count: %d, PENDING count: %d, ERROR count: %d, UNKNOWN count: %d\n",
			job.SuccessCount, job.FailureCount, job.AbortedCount, job.PendingCount, job.ErrorCount, job.UnknownCount)
		fmt.Printf("\t\t\tPASS RATE: %.0f%%\n", job.PassRate*100)
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

// jobResult carries one job's history back from a worker. Notes are collected
// rather than logged in place so the output stays in configuration order.
type jobResult struct {
	job     Presubmit
	charted bool
	notes   []string
	err     error
}

func analyzeJob(job Presubmit, resultsDepth int) jobResult {
	url := fmt.Sprintf("https://prow.ci.openshift.org/job-history/gs/origin-ci-test/pr-logs/directory/%s?buildId=", job.Name)
	successCount, failureCount, abortedCount, pendingCount, errorCount, unexpectedStatusCount, unknownCount, err := getJobHistory(url, resultsDepth)
	if err != nil {
		return jobResult{err: err}
	}

	result := jobResult{}
	totalJobCount := successCount + failureCount + abortedCount + pendingCount + errorCount + unknownCount
	if unexpectedStatusCount > 0 {
		result.notes = append(result.notes, fmt.Sprintf("warning: %d unrecognized build statuses for %s", unexpectedStatusCount, job.Name))
	}
	if totalJobCount == 0 {
		// job is configured but has no recent runs, so it has nothing to chart
		result.notes = append(result.notes, fmt.Sprintf("skipping %s: no build history", job.Name))
		return result
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
	job.Stage = stageFor(job)

	result.job = job
	result.charted = true
	return result
}

// stageFor reports when a job runs. OpenShift CI runs presubmits as a two
// stage pipeline: prow starts the first stage itself on every push, and the
// pipeline controller schedules the second stage once the first one passes.
// always_run is no longer a useful signal on its own, since almost every job
// now sets it to false and relies on one of the conditions below.
//
// The pipeline_ annotations deliberately do not appear here. They decide which
// second stage jobs are worth running against a particular pull request, not
// whether a job belongs to the second stage at all.
func stageFor(job Presubmit) string {
	// prow runs these directly, subject to its own file conditions
	if job.AlwaysRun || job.RunIfChanged != "" || job.SkipIfOnlyChanged != "" {
		return "1"
	}
	// the pipeline controller's required set is everything prow left alone that
	// still has to pass; optional jobs are never in it, so they wait to be asked
	// for by name
	if !job.Optional {
		return "2"
	}
	return "request"
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

// fetchJobHistoryPage retrieves one page of prow job history. prow answers with
// a 500 and a "failed to locate build data" message for a job that has never
// run, which is an ordinary result rather than a failure, so it is reported as
// an absent history. Anything else non-200 is prow being unavailable: retry,
// and give up loudly rather than let a job quietly vanish from the charts
// because prow was busy for a moment.
func fetchJobHistoryPage(url string) ([]byte, bool, error) {
	const attempts = 4

	var lastStatus string
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * time.Second)
		}

		resp, err := http.Get(url)
		if err != nil {
			lastStatus = err.Error()
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastStatus = err.Error()
			continue
		}

		if resp.StatusCode == http.StatusOK {
			return body, true, nil
		}
		if resp.StatusCode == http.StatusInternalServerError && bytes.Contains(body, []byte("failed to locate build data")) {
			return nil, false, nil
		}
		lastStatus = resp.Status
	}
	return nil, false, fmt.Errorf("GET %s failed after %d attempts, last result %s", url, attempts, lastStatus)
}

func processPage(url string, successCount *int, failureCount *int, abortedCount *int, pendingCount *int, errorCount *int, unexpectedStatusCount *int, unknownCount *int, depth int) error {
	if depth >= 0 {
		body, hasHistory, err := fetchJobHistoryPage(url)
		if err != nil {
			return err
		}
		if !hasHistory {
			// prow has no build data for this job, which means it has never run
			return nil
		}

		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
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
			return fmt.Errorf("no allBuilds data in the page at %s", url)
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
