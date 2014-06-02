package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/drone/drone/pkg/build/script"
	"github.com/drone/drone/pkg/database"
	. "github.com/drone/drone/pkg/model"
	"github.com/drone/drone/pkg/queue"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	droneYmlUrlPattern = "http://%s/%s/%s/raw/%s/.drone.yml"
)

type GogsHandler struct {
	queue *queue.Queue
}

func NewGogsHandler(queue *queue.Queue) *GogsHandler {
	return &GogsHandler{
		queue: queue,
	}
}

// Processes a generic POST-RECEIVE Gogs hook and
// attempts to trigger a build.
func (h *GogsHandler) Hook(w http.ResponseWriter, r *http.Request) error {

	defer r.Body.Close()
	payloadbytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		println(err.Error())
		return RenderText(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	}
	fmt.Printf("body is => %s\n", string(payloadbytes))

	payload, err := ParseHook(payloadbytes)
	if err != nil {
		println(err.Error())
		return RenderText(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	}
	fmt.Printf("payload parsed\n")

	// Verify that the commit doesn't already exist.
	// We should never build the same commit twice.
	_, err = database.GetCommitHash(payload.Commits[len(payload.Commits)-1].Id, payload.Repo.Id)
	if err != nil && err != sql.ErrNoRows {
		return RenderText(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	}
	fmt.Printf("commit hash checked\n")

	commit := &Commit{}
	commit.RepoID = payload.Repo.Id
	commit.Branch = payload.Branch()
	commit.Hash = payload.Commits[len(payload.Commits)-1].Id
	commit.Status = "Pending"
	commit.Created = time.Now().UTC()

	commit.Message = payload.Commits[len(payload.Commits)-1].Message
	commit.Timestamp = time.Now().UTC().String()
	commit.SetAuthor(payload.Commits[len(payload.Commits)-1].Author.Name)
	fmt.Printf("commit struct created\n")

	// save the commit to the database
	if err := database.SaveCommit(commit); err != nil {
		return RenderText(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
	fmt.Printf("commit struct saved\n")

	// save the build to the database
	build := &Build{}
	build.Slug = "1" // TODO
	build.CommitID = commit.ID
	build.Created = time.Now().UTC()
	build.Status = "Pending"
	if err := database.SaveBuild(build); err != nil {
		return RenderText(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
	println("build saved")
	var urlParts = strings.Split(payload.Repo.Url, "/")
	println("urlParts: ", urlParts)
	repo, err := NewRepo(urlParts[2], urlParts[3], urlParts[4], ScmGit, payload.Repo.Url)
	if err != nil {
		println(err.Error())
		return RenderText(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	}
	fmt.Printf("repo struct created\n")

	var service_endpoint = urlParts[2]
	if os.Getenv("GOGS_URL") != "" {
		service_endpoint = os.Getenv("GOGS_URL")
	}
	// GET .drone.yml file
	var droneYmlUrl = fmt.Sprintf(droneYmlUrlPattern, service_endpoint, urlParts[3], urlParts[4], commit.Hash)
	println("droneYmlUrl is ", droneYmlUrl)
	ymlGetResponse, err := http.Get(droneYmlUrl)
	var buildYml = ""
	if err != nil {
		println(err.Error())
		return RenderText(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	} else {
		defer ymlGetResponse.Body.Close()
		yml, err := ioutil.ReadAll(ymlGetResponse.Body)
		if err != nil {
			println(err.Error())
			return RenderText(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		buildYml := string(yml)
		println("yml from http get: ", buildYml)
	}

	// parse the build script
	var repoParams = map[string]string{}
	println("parsing yml")
	buildscript, err := script.ParseBuild([]byte(buildYml), repoParams)
	if err != nil {
		msg := "Could not parse your .drone.yml file.  It needs to be a valid drone yaml file.\n\n" + err.Error() + "\n"
		if err := saveFailedBuild(commit, msg); err != nil {
			return RenderText(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return RenderText(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	}
	fmt.Printf("build script parsed\n")

	// send the build to the queue
	h.queue.Add(&queue.BuildTask{Repo: repo, Commit: commit, Build: build, Script: buildscript})
	fmt.Printf("build task added to queue\n")

	// OK!
	return RenderText(w, http.StatusText(http.StatusOK), http.StatusOK)
}

type PayloadAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type PayloadCommit struct {
	Id      string         `json:"id"`
	Message string         `json:"message"`
	Url     string         `json:"url"`
	Author  *PayloadAuthor `json:"author"`
}

type PayloadRepo struct {
	Id          int64          `json:"id"`
	Name        string         `json:"name"`
	Url         string         `json:"url"`
	Description string         `json:"description"`
	Website     string         `json:"website"`
	Watchers    int            `json:"watchers"`
	Owner       *PayloadAuthor `json:"author"`
	Private     bool           `json:"private"`
}

// Payload represents payload information of payload.
type Payload struct {
	Secret  string           `json:"secret"`
	Ref     string           `json:"ref"`
	Commits []*PayloadCommit `json:"commits"`
	Repo    *PayloadRepo     `json:"repository"`
	Pusher  *PayloadAuthor   `json:"pusher"`
}

var ErrInvalidReceiveHook = errors.New("Invalid JSON payload received over webhook")

func ParseHook(raw []byte) (*Payload, error) {

	hook := Payload{}
	if err := json.Unmarshal(raw, &hook); err != nil {
		return nil, err
	}

	// it is possible the JSON was parsed, however,
	// was not from Github (maybe was from Bitbucket)
	// So we'll check to be sure certain key fields
	// were populated
	switch {
	case hook.Repo == nil:
		return nil, ErrInvalidReceiveHook
	case len(hook.Ref) == 0:
		return nil, ErrInvalidReceiveHook
	}

	return &hook, nil
}

func (h *Payload) Branch() string {
	return strings.Replace(h.Ref, "refs/heads/", "", -1)
}
