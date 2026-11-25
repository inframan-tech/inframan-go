// Command run-agent starts a run of an on-demand agent and waits for it to
// end, printing what it says as it goes.
//
//	INFRAMAN_URL=https://<console domain>/api INFRAMAN_INSTANCE_ID=<instance> INFRAMAN_TOKEN=<token> \
//	  go run ./examples/run-agent -agent <the agent's service ID> "Fix the failing test."
//
// INFRAMAN_TOKEN is a person's OAuth access token, as list-applications
// reads it (the README says how).
//
// Without INFRAMAN_TOKEN it signs in as the service account the workload runs
// as, as list-applications does (INFRAMAN_INSTANCE_ID).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	inframan "github.com/inframan-tech/inframan-go"
)

// run is what get_agent_run says of a run, as far as this program reads it.
type run struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	StatusDetail string `json:"status_detail"`
	Summary      string `json:"summary"`
	CostMicros   int64  `json:"cost_micros"`
	PullRequest  *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	Events []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		Tool string `json:"tool"`
	} `json:"events"`
	NextOffset int `json:"next_offset"`
}

// finished says a run's status is one it ends with.
func finished(status string) bool {
	switch status {
	case "queued", "running":
		return false
	}
	return true
}

func main() {
	agent := flag.String("agent", "", "the on-demand agent's service ID, from list_agents")
	wait := flag.Duration("wait", time.Hour, "how long to wait for the run")
	flag.Parse()
	if *agent == "" {
		log.Fatal("-agent is required")
	}
	base := os.Getenv("INFRAMAN_URL")
	instance := os.Getenv("INFRAMAN_INSTANCE_ID")
	tokens := inframan.GoogleWorkloadIdentity(base, instance)
	if tok := os.Getenv("INFRAMAN_TOKEN"); tok != "" {
		tokens = inframan.StaticToken(tok)
	}
	c, err := inframan.New(base, inframan.WithInstance(instance), inframan.WithTokenSource(tokens))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()

	p := inframan.RunAgentParams{ServiceID: *agent}
	if input := strings.Join(flag.Args(), " "); input != "" {
		p.Input = &input
	}
	raw, err := c.RunAgent(ctx, p)
	if err != nil {
		switch inframan.CodeOf(err) {
		case "infrastructure_asleep", "agents_not_ready", "queue_full":
			log.Fatalf("the run was refused (%s): %v", inframan.CodeOf(err), err)
		}
		log.Fatal(err)
	}
	var started struct{ Run run }
	if err := json.Unmarshal(raw, &started); err != nil {
		log.Fatal(err)
	}
	fmt.Println("run", started.Run.ID, started.Run.Status)

	offset := 0
	var last run
	err = inframan.Poll(ctx, 5*time.Second, func(ctx context.Context) (bool, error) {
		raw, err := c.GetAgentRun(ctx, inframan.GetAgentRunParams{ServiceID: *agent, RunID: started.Run.ID, Offset: &offset})
		if err != nil {
			return false, err
		}
		var got struct{ Run run }
		if err := json.Unmarshal(raw, &got); err != nil {
			return false, err
		}
		for _, e := range got.Run.Events {
			switch e.Kind {
			case "text":
				fmt.Println(e.Text)
			case "tool":
				fmt.Println("·", e.Tool)
			}
		}
		if got.Run.NextOffset >= 0 {
			offset = got.Run.NextOffset
		}
		last = got.Run
		return finished(got.Run.Status) && got.Run.NextOffset < 0, nil
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n%s: %s\n", last.Status, firstOf(last.Summary, last.StatusDetail))
	if last.PullRequest != nil {
		fmt.Println("pull request:", last.PullRequest.URL)
	}
	fmt.Printf("cost: $%.2f\n", float64(last.CostMicros)/1e6)
	if last.Status != "succeeded" {
		os.Exit(1)
	}
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
