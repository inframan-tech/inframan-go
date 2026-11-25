// Command list-applications lists an Inframan instance's applications.
//
// As a person, with the OAuth access token `inframan login --url <console>`
// keeps in ~/.inframan/credentials.json (the README says how to read it):
//
//	INFRAMAN_URL=https://<console domain>/api INFRAMAN_INSTANCE_ID=<instance> INFRAMAN_TOKEN=<token> \
//	  go run ./examples/list-applications
//
// As a service account, on a workload that runs as it (GKE Workload
// Identity, a VM's account), with no token at all:
//
//	INFRAMAN_URL=https://<console domain>/api INFRAMAN_INSTANCE_ID=<instance> go run ./examples/list-applications
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	inframan "github.com/inframan-tech/inframan-go"
)

func main() {
	base := os.Getenv("INFRAMAN_URL")
	instance := os.Getenv("INFRAMAN_INSTANCE_ID")
	var tokens inframan.TokenSource
	if tok := os.Getenv("INFRAMAN_TOKEN"); tok != "" {
		tokens = inframan.StaticToken(tok)
	} else {
		tokens = inframan.GoogleWorkloadIdentity(base, instance)
	}
	c, err := inframan.New(base, inframan.WithInstance(instance), inframan.WithTokenSource(tokens))
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, err := c.ListApplications(ctx, inframan.ListApplicationsParams{Status: inframan.Ptr("active")})
	if err != nil {
		if inframan.CodeOf(err) == "service_account_scope" {
			log.Fatal("this service account may call agent capabilities alone: ", err)
		}
		log.Fatal(err)
	}
	var apps []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Kind   string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &apps); err != nil {
		log.Fatal(err)
	}
	for _, a := range apps {
		fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Kind, a.Status)
	}
}
