package fis

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
)

const (
	// SeedTag carries an experiment's chaos seed. Set it on a template or
	// in StartExperiment's tags to replay a run; Stratus always reports the
	// seed it used under this tag.
	SeedTag = "stratus:seed"
)

// ChaosEngine is the port to the chaos engine; *chaos.Engine fits.
type ChaosEngine interface {
	Validate(spec chaos.Spec) error
	Start(spec chaos.Spec) (chaos.Report, error)
	Report() (chaos.Report, error)
	StopID(id string) (chaos.Report, error)
}

// Service implements the FIS API.
type Service struct {
	scope   scope.Scope
	mu      sync.Mutex
	repo    repository
	engine  ChaosEngine
	catalog map[string]ActionDefinition
	now     func() time.Time
	newID   func(prefix string) string
}

// NewService builds the FIS service.
func NewService(metadata store.Store, engine ChaosEngine) *Service {
	return &Service{
		scope:   scope.Default(),
		repo:    repository{store: metadata},
		engine:  engine,
		catalog: newCatalog(scope.Default()),
		now:     time.Now,
		newID:   newID,
	}
}

// TemplateInput is the body of CreateExperimentTemplate and, with every
// field optional, of UpdateExperimentTemplate.
type TemplateInput struct {
	ClientToken    string            `json:"clientToken,omitempty"`
	Description    *string           `json:"description,omitempty"`
	Targets        map[string]Target `json:"targets,omitempty"`
	Actions        map[string]Action `json:"actions,omitempty"`
	StopConditions []StopCondition   `json:"stopConditions,omitempty"`
	RoleArn        *string           `json:"roleArn,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
}

// CreateTemplate validates and stores a new template.
func (s *Service) CreateTemplate(in TemplateInput) (Template, error) {
	now := s.now().UTC()
	t := Template{
		ID:             s.newID("EXT"),
		Targets:        in.Targets,
		Actions:        in.Actions,
		StopConditions: in.StopConditions,
		Tags:           in.Tags,
		CreationTime:   Epoch(now),
		LastUpdateTime: Epoch(now),
	}
	t.Arn = fmt.Sprintf("arn:aws:fis:%s:%s:experiment-template/%s", s.scope.Region, s.scope.Account, t.ID)
	if in.Description != nil {
		t.Description = *in.Description
	}
	if in.RoleArn != nil {
		t.RoleArn = *in.RoleArn
	}
	normaliseTemplate(&t)
	if err := s.check(t); err != nil {
		return Template{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.repo.putTemplate(t); err != nil {
		return Template{}, internal(err)
	}
	return t, nil
}

// UpdateTemplate changes the fields the input sets. Targets and actions
// replace the template's whole map.
func (s *Service) UpdateTemplate(id string, in TemplateInput) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.template(id)
	if err != nil {
		return Template{}, err
	}
	if in.Description != nil {
		t.Description = *in.Description
	}
	if in.RoleArn != nil {
		t.RoleArn = *in.RoleArn
	}
	if in.Targets != nil {
		t.Targets = in.Targets
	}
	if in.Actions != nil {
		t.Actions = in.Actions
	}
	if in.StopConditions != nil {
		t.StopConditions = in.StopConditions
	}
	t.LastUpdateTime = Epoch(s.now().UTC())
	normaliseTemplate(&t)
	if err := s.check(t); err != nil {
		return Template{}, err
	}
	if err := s.repo.putTemplate(t); err != nil {
		return Template{}, internal(err)
	}
	return t, nil
}

// Template returns a template.
func (s *Service) Template(id string) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.template(id)
}

// Templates lists templates, newest first.
func (s *Service) Templates() ([]TemplateSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.repo.templates()
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreationTime.Time().After(all[j].CreationTime.Time()) })
	out := make([]TemplateSummary, 0, len(all))
	for _, t := range all {
		out = append(out, t.summary())
	}
	return out, nil
}

// DeleteTemplate deletes a template and returns it.
func (s *Service) DeleteTemplate(id string) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.template(id)
	if err != nil {
		return Template{}, err
	}
	if err := s.repo.deleteTemplate(id); err != nil {
		return Template{}, internal(err)
	}
	return t, nil
}

// StartInput is the body of StartExperiment.
type StartInput struct {
	ClientToken          string            `json:"clientToken,omitempty"`
	ExperimentTemplateID string            `json:"experimentTemplateId"`
	Tags                 map[string]string `json:"tags,omitempty"`
}

// StartExperiment runs a template. It replaces any running chaos
// experiment, FIS or not.
func (s *Service) StartExperiment(in StartInput) (Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.template(in.ExperimentTemplateID)
	if err != nil {
		return Experiment{}, err
	}
	tags := mergeTags(t.Tags, in.Tags)
	seed, err := seedFrom(tags)
	if err != nil {
		return Experiment{}, err
	}
	c, err := compile(t, s.catalog, seed)
	if err != nil {
		return Experiment{}, err
	}
	report, err := s.engine.Start(c.spec)
	if err != nil {
		return Experiment{}, fromChaos(err)
	}
	tags[SeedTag] = strconv.FormatInt(report.Seed, 10)

	now := s.now().UTC()
	actions := make(map[string]ExperimentAction, len(t.Actions))
	for name, a := range t.Actions {
		actions[name] = ExperimentAction{Action: a}
	}
	id := s.newID("EXP")
	rec := experimentRecord{
		Experiment: Experiment{
			ID:                   id,
			Arn:                  fmt.Sprintf("arn:aws:fis:%s:%s:experiment/%s", s.scope.Region, s.scope.Account, id),
			ExperimentTemplateID: t.ID,
			RoleArn:              t.RoleArn,
			Targets:              t.Targets,
			Actions:              actions,
			StopConditions:       t.StopConditions,
			CreationTime:         Epoch(now),
			StartTime:            Epoch(report.StartedAt),
			Tags:                 tags,
		},
		ChaosID: report.ID,
		Plan:    c.plan,
	}
	if err := s.repo.putExperiment(rec); err != nil {
		return Experiment{}, internal(err)
	}
	return rec.view(now), nil
}

// Experiment returns an experiment and its current state.
func (s *Service) Experiment(id string) (Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.experiment(id)
	if err != nil {
		return Experiment{}, err
	}
	return rec.view(s.now()), nil
}

// Experiments lists experiments, newest first.
func (s *Service) Experiments() ([]ExperimentSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.repo.experiments()
	if err != nil {
		return nil, internal(err)
	}
	now := s.now()
	current := s.currentChaosID()
	out := make([]ExperimentSummary, 0, len(all))
	for _, rec := range all {
		if rec.settle(now, rec.ChaosID == current) {
			_ = s.repo.putExperiment(rec)
		}
		out = append(out, rec.view(now).summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreationTime.Time().After(out[j].CreationTime.Time()) })
	return out, nil
}

// StopExperiment stops a running experiment.
func (s *Service) StopExperiment(id string) (Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.experiment(id)
	if err != nil {
		return Experiment{}, err
	}
	now := s.now()
	if rec.Final != nil {
		return Experiment{}, conflict("experiment %s has already ended (%s)", id, rec.Final.Status)
	}
	if _, err := s.engine.StopID(rec.ChaosID); err != nil {
		// The chaos run already ended; record why instead of failing.
		rec.settle(now, false)
	} else {
		rec.end(State{Status: StatusStopped, Reason: reasonStopped}, now)
	}
	if err := s.repo.putExperiment(rec); err != nil {
		return Experiment{}, internal(err)
	}
	return rec.view(now), nil
}

// Actions lists the supported actions.
func (s *Service) Actions() []ActionSummary {
	defs := sortedActions(s.catalog)
	out := make([]ActionSummary, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.summary())
	}
	return out
}

// Action describes one supported action.
func (s *Service) Action(id string) (ActionDefinition, error) {
	d, ok := s.catalog[id]
	if !ok {
		return ActionDefinition{}, notFound("action %s is not supported by Stratus", id)
	}
	return d, nil
}

// TargetResourceTypes lists the resource types templates can target.
func (s *Service) TargetResourceTypes() []TargetResourceType {
	return append([]TargetResourceType(nil), targetResourceTypes...)
}

// check validates a template by compiling it and asking the engine to
// validate the rules, so a bad template fails at create time, not start.
func (s *Service) check(t Template) error {
	c, err := compile(t, s.catalog, 1)
	if err != nil {
		return err
	}
	return fromChaos(s.engine.Validate(c.spec))
}

func (s *Service) template(id string) (Template, error) {
	t, err := s.repo.template(id)
	if errors.Is(err, errNotFound) {
		return Template{}, notFound("experiment template %s not found", id)
	}
	if err != nil {
		return Template{}, internal(err)
	}
	return t, nil
}

// experiment loads an experiment and settles it if it has ended.
func (s *Service) experiment(id string) (experimentRecord, error) {
	rec, err := s.repo.experiment(id)
	if errors.Is(err, errNotFound) {
		return experimentRecord{}, notFound("experiment %s not found", id)
	}
	if err != nil {
		return experimentRecord{}, internal(err)
	}
	if rec.settle(s.now(), rec.ChaosID == s.currentChaosID()) {
		if err := s.repo.putExperiment(rec); err != nil {
			return experimentRecord{}, internal(err)
		}
	}
	return rec, nil
}

func (s *Service) currentChaosID() string {
	report, err := s.engine.Report()
	if err != nil {
		return ""
	}
	return report.ID
}

func normaliseTemplate(t *Template) {
	if t.Targets == nil {
		t.Targets = map[string]Target{}
	}
	if t.StopConditions == nil {
		t.StopConditions = []StopCondition{{Source: "none"}}
	}
	for name, target := range t.Targets {
		if target.SelectionMode == "" {
			target.SelectionMode = "ALL"
			t.Targets[name] = target
		}
	}
}

func mergeTags(sets ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, set := range sets {
		for k, v := range set {
			out[k] = v
		}
	}
	return out
}

func seedFrom(tags map[string]string) (int64, error) {
	raw, ok := tags[SeedTag]
	if !ok || raw == "" {
		return 0, nil // the engine picks one and reports it
	}
	seed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, validation("tag %s must be an integer, got %q", SeedTag, raw)
	}
	return seed, nil
}

// fromChaos turns a chaos validation error into an FIS one.
func fromChaos(err error) error {
	var ce *control.Error
	if errors.As(err, &ce) {
		return validation("%s", ce.Message)
	}
	return err
}

const idAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// newID makes an FIS-style ID: the prefix and 14 random characters.
func newID(prefix string) string {
	var b [14]byte
	_, _ = rand.Read(b[:])
	var sb strings.Builder
	sb.WriteString(prefix)
	for _, c := range b {
		sb.WriteByte(idAlphabet[int(c)%len(idAlphabet)])
	}
	return sb.String()
}

// SetScope sets the account and region this service instance serves.
func (s *Service) SetScope(sc scope.Scope) {
	s.scope = sc
	s.catalog = newCatalog(sc)
}
