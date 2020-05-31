package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerctx"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/event"
	"github.com/olivere/elastic/v7"
)

type eventDoc struct {
	BuildID      int              `json:"build_id"`
	BuildName    string           `json:"build_name"`
	JobID        int              `json:"job_id"`
	JobName      string           `json:"job_name"`
	PipelineID   int              `json:"pipeline_id"`
	PipelineName string           `json:"pipeline_name"`
	TeamID       int              `json:"team_id"`
	TeamName     string           `json:"team_name"`
	EventType    atc.EventType    `json:"event"`
	Version      atc.EventVersion `json:"version"`
	Data         *json.RawMessage `json:"data"`
	Tiebreak     int64            `json:"tiebreak"`
}

type Key struct {
	TimeMillis int64 `json:"time"`
	Tiebreak   int64 `json:"tiebreak"`
}

func (k Key) Marshal() ([]byte, error) {
	return json.Marshal(k)
}

func (k Key) GreaterThan(o db.EventKey) bool {
	if o == nil {
		return true
	}
	other, ok := o.(Key)
	if !ok {
		return false
	}
	if k.TimeMillis > other.TimeMillis {
		return true
	}
	if k.TimeMillis < other.TimeMillis {
		return false
	}
	return k.Tiebreak > other.Tiebreak
}

type Store struct {
	logger lager.Logger
	client *elastic.Client

	URL string `long:"url" description:"URL of Elasticsearch cluster."`

	counter int64
}

func (e *Store) IsConfigured() bool {
	return e.URL != ""
}

func (e *Store) Setup(ctx context.Context) error {
	e.logger = lagerctx.FromContext(ctx)

	e.logger.Debug("setup-event-store", lager.Data{"url": e.URL})
	var err error
	e.client, err = elastic.NewClient(
		elastic.SetURL(e.URL),
		elastic.SetHealthcheckTimeoutStartup(1 * time.Minute),
	)
	if err != nil {
		e.logger.Error("connect-to-cluster-failed", err, lager.Data{"url": e.URL})
		return fmt.Errorf("connect to cluster: %w", err)
	}

	_, err = e.client.XPackIlmPutLifecycle().
		Policy(ilmPolicyName).
		BodyString(ilmPolicyJSON).
		Do(ctx)
	if err != nil {
		e.logger.Error("put-ilm-policy-failed", err, lager.Data{"name": ilmPolicyName, "json": ilmPolicyJSON})
		return fmt.Errorf("put ilm policy: %w", err)
	}

	_, err = e.client.IndexPutTemplate(indexTemplateName).
		BodyString(indexTemplateJSON).
		Do(ctx)
	if err != nil {
		e.logger.Error("put-index-template-failed", err, lager.Data{"name": indexTemplateName, "json": indexTemplateJSON})
		return fmt.Errorf("put index template: %w", err)
	}

	err = e.createIndexIfNotExists(ctx, initialIndexName, initialIndexJSON)
	if err != nil {
		e.logger.Error("create-initial-index-failed", err, lager.Data{"name": initialIndexName, "json": initialIndexJSON})
		return fmt.Errorf("create initial index: %w", err)
	}

	return nil
}

func (e *Store) Close(ctx context.Context) error {
	e.client.Stop()
	return nil
}

func (e *Store) createIndexIfNotExists(ctx context.Context, name string, body string) error {
	exists, err := e.client.IndexExists(name).Do(ctx)
	if err != nil {
		e.logger.Error("check-index-exists-failed", err, lager.Data{"name": name})
		return fmt.Errorf("check index exists: %w", err)
	}
	if exists {
		return nil
	}
	_, err = e.client.CreateIndex(name).Body(body).Do(ctx)
	if err != nil && !isAlreadyExists(err) {
		e.logger.Error("create-index-failed", err, lager.Data{"name": name})
		if isAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

func isAlreadyExists(err error) bool {
	elasticErr, ok := err.(*elastic.Error)
	if !ok {
		return false
	}
	return elasticErr.Status == http.StatusBadRequest && elasticErr.Details.Type == "index_already_exists_exception"
}

func (e *Store) Initialize(ctx context.Context, build db.Build) error {
	return nil
}

func (e *Store) Finalize(ctx context.Context, build db.Build) error {
	return nil
}

func (e *Store) Put(ctx context.Context, build db.Build, events []atc.Event) (db.EventKey, error) {
	if len(events) == 0 {
		return nil, nil
	}
	bulkRequest := e.client.Bulk()
	var doc eventDoc
	for _, evt := range events {
		payload, err := json.Marshal(evt)
		if err != nil {
			e.logger.Error("marshal-event-failed", err)
			return nil, fmt.Errorf("marshal event: %w", err)
		}
		data := json.RawMessage(payload)
		doc = eventDoc{
			BuildID:      build.ID(),
			BuildName:    build.Name(),
			JobID:        build.JobID(),
			JobName:      build.JobName(),
			PipelineID:   build.PipelineID(),
			PipelineName: build.PipelineName(),
			TeamID:       build.TeamID(),
			TeamName:     build.TeamName(),
			EventType:    evt.EventType(),
			Version:      evt.Version(),
			Data:         &data,
			Tiebreak:     atomic.AddInt64(&e.counter, 1),
		}
		bulkRequest = bulkRequest.Add(
			elastic.NewBulkIndexRequest().
				Index(indexPatternPrefix).
				Doc(doc),
		)
	}
	_, err := bulkRequest.Do(ctx)
	if err != nil {
		e.logger.Error("bulk-put-failed", err)
		return nil, fmt.Errorf("bulk put: %w", err)
	}

	var target struct {
		Time int64 `json:"time"`
	}
	if err = json.Unmarshal(*doc.Data, &target); err != nil {
		return nil, err
	}

	return Key{TimeMillis: target.Time * 1000, Tiebreak: doc.Tiebreak}, nil
}

func (e *Store) Get(ctx context.Context, build db.Build, requested int, cursor *db.EventKey) ([]event.Envelope, error) {
	offset, err := e.offset(cursor)
	if err != nil {
		e.logger.Error("offset-failed", err)
		return nil, err
	}

	req := e.client.Search(indexPatternPrefix).
		Query(elastic.NewTermQuery("build_id", build.ID())).
		Sort("data.time", true).
		Sort("tiebreak", true).
		Size(requested)
	if offset.TimeMillis > 0 {
		req = req.SearchAfter(offset.TimeMillis, offset.Tiebreak)
	}

	searchResult, err := req.Do(ctx)
	if err != nil {
		e.logger.Error("search-failed", err)
		return nil, fmt.Errorf("perform search: %w", err)
	}

	numHits := len(searchResult.Hits.Hits)
	if numHits == 0 {
		return []event.Envelope{}, nil
	}
	events := make([]event.Envelope, numHits)
	for i, hit := range searchResult.Hits.Hits {
		var envelope event.Envelope
		if err = json.Unmarshal(hit.Source, &envelope); err != nil {
			e.logger.Error("unmarshal-hit-failed", err)
			return nil, fmt.Errorf("unmarshal source to event.Envelope: %w", err)
		}
		events[i] = envelope
	}

	lastHit := searchResult.Hits.Hits[numHits-1]
	var target struct {
		Tiebreak int64 `json:"tiebreak"`
		Data     struct {
			Time int64 `json:"time"`
		} `json:"data"`
	}
	if err = json.Unmarshal(lastHit.Source, &target); err != nil {
		e.logger.Error("unmarshal-last-hit-failed", err)
		return nil, fmt.Errorf("unmarshal last hit: %w", err)
	}
	*cursor = Key{
		TimeMillis: target.Data.Time * 1000,
		Tiebreak:   target.Tiebreak,
	}

	return events, nil
}

func (e *Store) offset(cursor *db.EventKey) (Key, error) {
	if cursor == nil || *cursor == nil {
		return Key{}, nil
	}
	offset, ok := (*cursor).(Key)
	if !ok {
		return Key{}, fmt.Errorf("invalid Key type (expected elasticsearch.Key, got %T)", *cursor)
	}
	return offset, nil
}

func (e *Store) Delete(ctx context.Context, builds []db.Build) error {
	buildIDs := make([]int, len(builds))
	for i, build := range builds {
		buildIDs[i] = build.ID()
	}
	err := e.asyncDelete(ctx, elastic.NewTermsQuery("build_id", buildIDs))
	if err != nil {
		e.logger.Error("delete-builds-failed", err, lager.Data{"build_ids": buildIDs})
		return fmt.Errorf("delete builds: %w", err)
	}
	return nil
}

func (e *Store) DeletePipeline(ctx context.Context, pipeline db.Pipeline) error {
	err := e.asyncDelete(ctx, elastic.NewTermQuery("pipeline_id", pipeline.ID()))
	if err != nil {
		e.logger.Error("delete-pipeline-failed", err, lager.Data{"pipeline_id": pipeline.ID()})
		return fmt.Errorf("delete pipeline: %w", err)
	}
	return nil
}

func (e *Store) DeleteTeam(ctx context.Context, team db.Team) error {
	err := e.asyncDelete(ctx, elastic.NewTermQuery("team_id", team.ID()))
	if err != nil {
		e.logger.Error("delete-team-failed", err, lager.Data{"team_id": team.ID()})
		return fmt.Errorf("delete team: %w", err)
	}
	return nil
}

func (e *Store) asyncDelete(ctx context.Context, query elastic.Query) error {
	_, err := e.client.DeleteByQuery(indexPatternPrefix).
		Query(query).
		DoAsync(ctx)
	return err
}

func (e *Store) UnmarshalKey(data []byte, key *db.EventKey) error {
	var k Key
	if err := json.Unmarshal(data, &k); err != nil {
		return err
	}
	*key = k
	return nil
}
