package googutil

import (
	"golang.org/x/oauth2/google"
	"net/http"
	"context"
	"fmt"
)

func NewGoogleClientOrDie() *http.Client {
	googleContext := context.Background()
	var client *http.Client
	var err error
	if client, err = google.DefaultClient(googleContext); err != nil {
		panic(fmt.Sprintf("Error instantiating Google API client: %s", err))
	}

	return client
}