package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
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
		fmt.Println("Usage: go run main.go <org> <repo> <start-date> <end-date>")
		return
	}

	owner := os.Args[1]
	repo := os.Args[2]
	startDate := os.Args[3]
	endDate := os.Args[4]

	startTime, err := time.Parse("01-02-2006", startDate)
	if err != nil {
		fmt.Println("Invalid start date:", err)
		return
	}

	endTime, err := time.Parse("01-02-2006", endDate)
	if err != nil {
		fmt.Println("Invalid end date:", err)
		return
	}

	pullRequests, err := getClosedPullRequests(owner, repo, startTime, endTime)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	fmt.Printf("Pull Requests closed between %s and %s:\n", startTime, endTime)
	for _, pr := range pullRequests {
		var PRJobInfo []JobInfo

		awsTotalHours := 0.0
		gcpTotalHours := 0.0
		vsphereTotalHours := 0.0
		azureTotalHours := 0.0

		org, repo, prNum, _ := extractPRInfo(pr.URL)
		prowJobURL := generateProwJobURL(org, repo, prNum)
		prJobLinks, _ := parseProwJobURL(prowJobURL)
		fmt.Printf("%s/%s PR #%d ran jobs:\n", org, repo, prNum)
		for _, prJobLink := range prJobLinks {
			jobInfo := JobInfo{
				JobURL:   "",
				Duration: 0,
				Cost:     0,
			}
			decimalHours := 0.0
			if strings.Contains(prJobLink, "aws") || strings.Contains(prJobLink, "gcp") || strings.Contains(prJobLink, "vsphere") {
				parsedPrJobUrl, _ := url.Parse(prJobLink)
				pathSegments := strings.Split(parsedPrJobUrl.Path, "/")
				jobID := pathSegments[len(pathSegments)-1]
				jobName := pathSegments[len(pathSegments)-2]
				buildLogURL := fmt.Sprintf("https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/origin-ci-test/pr-logs/pull/%s_%s/%d/%s/%s/build-log.txt", org, repo, prNum, jobName, jobID)
				resp, _ := http.Get(buildLogURL)
				// TODO: add error checking here ^^ and below
				body, _ := io.ReadAll(resp.Body)
				buildLogContent := string(body)
				runTimePattern := `Ran for (\d+)h(\d+)m`
				regex := regexp.MustCompile(runTimePattern)
				match := regex.FindStringSubmatch(buildLogContent)
				if len(match) < 3 {
					fmt.Errorf("failed to extract run time from build log")
				} else {
					hours, _ := strconv.Atoi(match[1])
					minutes, _ := strconv.Atoi(match[2])

					// Convert the run time to decimal hours
					decimalHours = float64(hours) + float64(minutes)/60.0
					fmt.Printf("\t%s/%s for %.2f hours\n", jobName, jobID, decimalHours)
				}
				// remove 30m to estimate the time for actual cloud nodes to be provisioned, just don't let it go
				// negative
				decimalHours -= 0.5
				if decimalHours < 0.0 {
					decimalHours = 0.0
				}

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
						fmt.Printf("Unable to calculate costs for %s\n", prJobLink)
					}
					fmt.Printf("Need to calculate a cost for #{prJobLink}\n")
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
		prInfo := PRInfo{
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
			jamo := nextURL["rel=\"next"]
			fmt.Println(jamo)
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
