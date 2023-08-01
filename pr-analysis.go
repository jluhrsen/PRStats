package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type PullRequest struct {
	Title     string    `json:"title"`
	URL       string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	ClosedAt  time.Time `json:"closed_at"`
}
type Comment struct {
	Body string `json:"body"`
}

type JobInfo struct {
	JobURL   string
	Duration float64
	Cost     float64
}

type PRInfo struct {
	Org               string
	Repo              string
	PRNum             int
	PRLifeSpan        float64
	PRRetestCount     int
	Jobs              []JobInfo
	AWSTotalHours     float64
	GCPTotalHours     float64
	VsphereTotalHours float64
	AzureTotalHours   float64
	TotalCost         float64
}

var prInfoSlice []PRInfo

var (
	awsCostRate     = 0.90
	gcpCostRate     = 1.70
	vsphereCostRate = 4.10
	azureCostRate   = 2.30
)

func main() {
	if len(os.Args) < 4 {
		log.Fatalf("Usage: go run main.go <org> <repo> <start-date> <end-date>")
	}

	owner, repo, startTime, endTime, err := parseArgs(os.Args[1:])

	if err != nil {
		log.Fatalf("Failed to parse arguments: %v", err)
	}

	pullRequests, err := getClosedPullRequests(owner, repo, startTime, endTime)
	if err != nil {
		log.Fatalf("Failed to get pull requests: %v", err)
	}

	processPullRequests(pullRequests, startTime, endTime)
}

func parseArgs(args []string) (string, string, time.Time, time.Time, error) {
	owner := args[0]
	repo := args[1]
	startDate, err := parseDate(args[2])
	if err != nil {
		return "", "", time.Time{}, time.Time{}, fmt.Errorf("invalid start date: %v", err)
	}
	endDate, err := parseDate(args[3])
	if err != nil {
		return "", "", time.Time{}, time.Time{}, fmt.Errorf("invalid end date: %v", err)
	}
	return owner, repo, startDate, endDate, nil
}

func parseDate(date string) (time.Time, error) {
	return time.Parse("01-02-2006", date)
}

func processPullRequests(pullRequests []PullRequest, startTime, endTime time.Time) {

	const maxGoroutines = 10
	semaphore := make(chan struct{}, maxGoroutines)

	prInfoChan := make(chan PRInfo, len(pullRequests))

	fmt.Printf("Pull Requests closed between %s and %s:\n", startTime, endTime)
	for _, pr := range pullRequests {
		semaphore <- struct{}{}

		go func(pr PullRequest) {
			var PRJobInfo []JobInfo

			awsTotalHours := 0.0
			gcpTotalHours := 0.0
			vsphereTotalHours := 0.0
			azureTotalHours := 0.0

			org, repo, prNum, _ := extractPRInfo(pr.URL)
			prowJobURL := generateProwJobURL(org, repo, prNum)
			prJobLinks, _ := parseProwJobURL(prowJobURL)
			fmt.Printf("%s/%s PR #%d:\n", org, repo, prNum)
			for _, prJobLink := range prJobLinks {
				jobInfo := JobInfo{
					JobURL:   "",
					Duration: 0,
					Cost:     0,
				}
				if strings.Contains(prJobLink, "aws") || strings.Contains(prJobLink, "gcp") || strings.Contains(prJobLink, "vsphere") {
					parsedPrJobUrl, _ := url.Parse(prJobLink)
					pathSegments := strings.Split(parsedPrJobUrl.Path, "/")
					jobID := pathSegments[len(pathSegments)-1]
					jobName := pathSegments[len(pathSegments)-2]
					decimalHours := getJobRunTime(org, repo, prNum, jobName, jobID)

					if strings.Contains(prJobLink, "aws") {
						awsTotalHours += decimalHours
						jobInfo = JobInfo{
							JobURL:   prJobLink,
							Duration: decimalHours,
							Cost:     decimalHours * awsCostRate,
						}
					} else if strings.Contains(prJobLink, "gcp") {
						gcpTotalHours += decimalHours
						jobInfo = JobInfo{
							JobURL:   prJobLink,
							Duration: decimalHours,
							Cost:     decimalHours * gcpCostRate,
						}
					} else if strings.Contains(prJobLink, "vsphere") {
						vsphereTotalHours += decimalHours
						jobInfo = JobInfo{
							JobURL:   prJobLink,
							Duration: decimalHours,
							Cost:     decimalHours * vsphereCostRate,
						}
					} else if strings.Contains(prJobLink, "azure") {
						azureTotalHours += decimalHours
						jobInfo = JobInfo{
							JobURL:   prJobLink,
							Duration: decimalHours,
							Cost:     decimalHours * azureCostRate,
						}
					} else {
						// we know we don't care about the "images", "lint", "unit" or "gofmt" jobs
						if !strings.Contains(prJobLink, "images") && !strings.Contains(prJobLink, "lint") &&
							!strings.Contains(prJobLink, "unit") && !strings.Contains(prJobLink, "gofmt") {
							// fmt.Printf("Unable to calculate costs for %s\n", prJobLink)
						}
						fmt.Printf("Unknown job type, cannot calculate costs %s\n", prJobLink)
						jobInfo = JobInfo{
							JobURL:   prJobLink,
							Duration: decimalHours,
							Cost:     0,
						}
					}
				}
				PRJobInfo = append(PRJobInfo, jobInfo)
			}

			prLifespan := pr.ClosedAt.Sub(pr.CreatedAt).Hours() / 24
			prComments, _ := getPRComments(org, repo, prNum)
			prRetestCount := 0
			for _, comment := range prComments {
				prRetestCount += countRetestsInComments(comment.Body, "/retest", "/retest-required")
			}
			awsTotalCost := awsTotalHours * awsCostRate
			gcpTotalCost := gcpTotalHours * gcpCostRate
			vsphereTotalCost := vsphereTotalHours * vsphereCostRate
			azureTotalCost := azureTotalHours * azureCostRate
			totalCloudCosts := awsTotalCost + gcpTotalCost + vsphereTotalCost + azureTotalCost
			prInfoChan <- PRInfo{
				Org:               org,
				Repo:              repo,
				PRNum:             prNum,
				PRLifeSpan:        prLifespan,
				PRRetestCount:     prRetestCount,
				Jobs:              PRJobInfo,
				AWSTotalHours:     awsTotalHours,
				GCPTotalHours:     gcpTotalHours,
				VsphereTotalHours: vsphereTotalHours,
				AzureTotalHours:   azureTotalHours,
				TotalCost:         totalCloudCosts,
			}
			<-semaphore
		}(pr)
	}
	for range pullRequests {
		prInfo := <-prInfoChan
		// Append the PRInfo to the slice
		prInfoSlice = append(prInfoSlice, prInfo)
	}
	sort.Slice(prInfoSlice, func(i, j int) bool {
		return prInfoSlice[i].TotalCost > prInfoSlice[j].TotalCost
	})
	jsonData, err := json.Marshal(prInfoSlice)
	if err != nil {
		log.Fatalf("Failed to marshal prInfoSlice to JSON: %v", err)
	}

	// Write JSON data to a file
	file, err := os.Create("pr_costs.json")
	if err != nil {
		log.Fatalf("Failed to create file: %v", err)
	}
	defer file.Close()

	_, err = file.Write(jsonData)
	if err != nil {
		log.Fatalf("Failed to write JSON data to file: %v", err)
	}

	fmt.Println("PR Costs (sorted from most expensive to least):")
	for _, prInfo := range prInfoSlice {
		fmt.Printf(`
	TOTAL PR COST:  $%.2f
	TOTAL CLOUD USAGE FOR PR %s/%s/%d
		AWS
			HOURS: %.2f
			COSTS: $%.2f
		GCP
			HOURS: %.2f
			COSTS: $%.2f
		VSPHERE
			HOURS: %.2f
			COSTS: $%.2f
		AZURE
			HOURS: %.2f
			COSTS: $%.2f
`,
			prInfo.TotalCost, prInfo.Org, prInfo.Repo, prInfo.PRNum, prInfo.AWSTotalHours, awsCostRate*prInfo.AWSTotalHours,
			prInfo.GCPTotalHours, gcpCostRate*prInfo.GCPTotalHours, prInfo.VsphereTotalHours,
			vsphereCostRate*prInfo.VsphereTotalHours, prInfo.AzureTotalHours, azureCostRate*prInfo.AzureTotalHours)
	}

}

func generateProwJobURL(org, repo string, prNum int) string {
	baseURL := "https://prow.ci.openshift.org/pr-history/?org=%s&repo=%s&pr=%d"
	return fmt.Sprintf(baseURL, org, repo, prNum)
}

func parseProwJobURL(prURL string) ([]string, error) {
	resp, err := http.Get(prURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch URL: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %s", err)
	}

	jobURLs := make([]string, 0)
	jobURLPattern := `/view/gs/origin-ci-test/pr-logs/pull/.*/\d+/.*/\d+`
	regex := regexp.MustCompile(jobURLPattern)
	matches := regex.FindAllStringSubmatch(string(body), -1)
	for _, match := range matches {
		jobURLs = append(jobURLs, "https://prow.ci.openshift.org"+match[0])
	}

	return jobURLs, nil
}

func extractPRInfo(prURL string) (string, string, int, error) {
	// Remove the leading "https://github.com/" from the URL
	prURL = strings.TrimPrefix(prURL, "https://github.com/")

	// Split the URL path into segments
	segments := strings.Split(prURL, "/")

	// Extract the organization, repository, and PR number from the segments
	org := segments[0]
	repo := segments[1]
	prStr := segments[3]

	// Parse the PR number as an integer
	prNum, err := strconv.Atoi(prStr)
	if err != nil {
		return "", "", 0, err
	}

	return org, repo, prNum, nil
}

func getClosedPullRequests(owner, repo string, startTime, endTime time.Time) ([]PullRequest, error) {
	var allPullRequests []PullRequest
	page := 1

	for {
		baseURL := fmt.Sprintf("https://api.github.com/search/issues?q=repo:%s/%s+is:pr+is:closed+closed:%s..%s&page=%d", owner, repo, startTime.Format("2006-01-02"), endTime.Format("2006-01-02"), page)

		req, err := http.NewRequest("GET", baseURL, nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("Accept", "application/vnd.github.v3+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected response status: %s", resp.Status)
		}

		var result struct {
			Items []PullRequest `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}

		filteredPullRequests := filterByCreationDate(result.Items, startTime)

		allPullRequests = append(allPullRequests, filteredPullRequests...)

		linkHeader := resp.Header.Get("Link")
		nextURL := parseLinkHeader(linkHeader)
		// TODO: this is ugly. need to fix in parseLinkHeader
		if nextURL["rel=\"next"] == "" {
			break
		}

		page++
		baseURL = nextURL["next"]
	}

	return allPullRequests, nil
}

func filterByCreationDate(pullRequests []PullRequest, startTime time.Time) []PullRequest {
	filtered := make([]PullRequest, 0)
	sixMonthsAgo := startTime.AddDate(0, -6, 0)

	for _, pr := range pullRequests {
		if pr.CreatedAt.After(sixMonthsAgo) {
			filtered = append(filtered, pr)
		}
	}

	return filtered
}

func parseLinkHeader(header string) map[string]string {
	links := make(map[string]string)
	sections := strings.Split(header, ",")
	for _, section := range sections {
		parts := strings.Split(strings.TrimSpace(section), ";")
		if len(parts) < 2 {
			continue
		}
		urlPart := strings.Trim(parts[0], "<>")
		relPart := strings.Trim(parts[1], `" `)
		links[relPart] = urlPart
	}
	return links
}

func getPRComments(owner, repo string, prNumber int) ([]Comment, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, prNumber)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected response status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var comments []Comment
	if err := json.Unmarshal(body, &comments); err != nil {
		return nil, err
	}

	return comments, nil
}

func countRetestsInComments(text string, targetStrings ...string) int {
	count := 0

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		for _, target := range targetStrings {
			if line == target {
				count++
			}
		}
	}

	return count
}

// in some cases the job could fail or abort and the started and/or finished json files may not be present
// marking runtime as -1.0 in those cases
func getJobRunTime(org, repo string, prNum int, jobName, jobID string) float64 {

	startJsonUrl := fmt.Sprintf("https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/origin-ci-test/pr-logs/pull/%s_%s/%d/%s/%s/started.json", org, repo, prNum, jobName, jobID)
	finishJsonUrl := fmt.Sprintf("https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/origin-ci-test/pr-logs/pull/%s_%s/%d/%s/%s/finished.json", org, repo, prNum, jobName, jobID)
	startInfo, err := http.Get(startJsonUrl)
	if err != nil {
		return -1.0
	}
	defer startInfo.Body.Close()
	finishInfo, err := http.Get(finishJsonUrl)
	if err != nil {
		return -1.0
	}
	defer finishInfo.Body.Close()
	startBody, _ := io.ReadAll(startInfo.Body)
	finishBody, _ := io.ReadAll(finishInfo.Body)

	var result map[string]interface{}

	err = json.Unmarshal([]byte(startBody), &result)
	if err != nil {
		return -1.0
	}
	startTime := result["timestamp"]

	err = json.Unmarshal([]byte(finishBody), &result)
	if err != nil {
		return -1.0
	}
	endTime := result["timestamp"]

	jobRunTime := endTime.(float64) - startTime.(float64)
	jobRunTimeHours := jobRunTime / 3600

	// remove 30m to estimate the time for actual cloud nodes to be provisioned, just don't let it go negative
	jobRunTimeHours -= 0.5
	if jobRunTimeHours < 0.0 {
		jobRunTimeHours = 0.0
	}

	// round the duration to one decimal point
	return math.Round(jobRunTimeHours*10) / 10

}
