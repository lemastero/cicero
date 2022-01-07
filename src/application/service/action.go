package service

import (
	"fmt"

	"cuelang.org/go/cue"
	"github.com/google/uuid"
	nomad "github.com/hashicorp/nomad/api"
	"github.com/jackc/pgx/v4"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/input-output-hk/cicero/src/application"
	"github.com/input-output-hk/cicero/src/config"
	"github.com/input-output-hk/cicero/src/domain"
	"github.com/input-output-hk/cicero/src/domain/repository"
	"github.com/input-output-hk/cicero/src/infrastructure/persistence"
)

type ActionService interface {
	GetById(uuid.UUID) (domain.Action, error)
	GetLatestByName(string) (domain.Action, error)
	GetAll() ([]*domain.Action, error)
	GetCurrent(pgx.Tx) ([]*domain.Action, error)
	Save(pgx.Tx, *domain.Action) error
	IsRunnable(pgx.Tx, *domain.Action) (bool, map[string]interface{}, error)
	Create(pgx.Tx, string, string) (*domain.Action, error)
	Invoke(pgx.Tx, *domain.Action) error
}

type actionService struct {
	logger            zerolog.Logger
	actionRepository  repository.ActionRepository
	factRepository    repository.FactRepository
	evaluationService EvaluationService
	runService        RunService
	nomadClient       application.NomadClient
}

func NewActionService(db config.PgxIface, nomadClient application.NomadClient, runService RunService, evaluationService EvaluationService, logger *zerolog.Logger) ActionService {
	return &actionService{
		logger:            logger.With().Str("component", "ActionService").Logger(),
		actionRepository:  persistence.NewActionRepository(db),
		factRepository:    persistence.NewFactRepository(db),
		evaluationService: evaluationService,
		nomadClient:       nomadClient,
		runService:        runService,
	}
}

func (self *actionService) GetById(id uuid.UUID) (action domain.Action, err error) {
	self.logger.Debug().Str("id", id.String()).Msg("Getting Action by ID")
	action, err = self.actionRepository.GetById(id)
	err = errors.WithMessagef(err, "Could not select existing Action for ID %q", id)
	return
}

func (self *actionService) GetLatestByName(name string) (action domain.Action, err error) {
	self.logger.Debug().Str("name", name).Msg("Getting latest Action by name")
	action, err = self.actionRepository.GetLatestByName(name)
	err = errors.WithMessagef(err, "Could not select latest Action for name %q", name)
	return
}

func (self *actionService) GetAll() ([]*domain.Action, error) {
	self.logger.Debug().Msg("Getting all Actions")
	return self.actionRepository.GetAll()
}

func (self *actionService) Save(tx pgx.Tx, action *domain.Action) error {
	self.logger.Debug().Str("name", action.Name).Msg("Saving new Action")
	if err := self.actionRepository.Save(tx, action); err != nil {
		return errors.WithMessagef(err, "Could not insert Action")
	}
	self.logger.Debug().Str("id", action.ID.String()).Msg("Created Action")
	return nil
}

func (self *actionService) GetCurrent(tx pgx.Tx) (actions []*domain.Action, err error) {
	self.logger.Debug().Msg("Getting current Actions")
	actions, err = self.actionRepository.GetCurrent(tx)
	err = errors.WithMessagef(err, "Could not select current Actions")
	return
}

func (self *actionService) IsRunnable(tx pgx.Tx, action *domain.Action) (bool, map[string]interface{}, error) {
	self.logger.Debug().Str("name", action.Name).Str("id", action.ID.String()).Msg("Checking whether Action is runnable")

	inputFact := map[string]*domain.Fact{}
	inputFacts := map[string][]*domain.Fact{}

	inputs := map[string]interface{}{}

	// XXX We only check that the required paths are present.
	// In the future we could extend this to also check some values.
	// For that we would have to convert CUE constraints to SQL clauses
	// which sounds non-trivial and it would only help improve performance,
	// except we define a subset of constraints as the only conditions we support
	// and no longer run the CUE filter over the selected facts afterwards.
	// But heads up! This would mean that inputs' `select: "latest"` behavior
	// would be changed, as what is latest currently means "latest with these paths"
	// but would then mean "latest with these paths AND matching values", so beware!

	// FIXME race condition: facts may change in between checking whether each input is satisfied
	for name, input := range action.Inputs {
		switch input.Select {
		case domain.InputDefinitionSelectLatest:
			if fact, err := self.getInputFactLatest(tx, input.Match.WithoutInputs()); err != nil {
				return false, nil, err
			} else if fact == nil {
				if !input.Not && !input.Optional {
					return false, nil, nil
				}
			} else {
				inputFact[name] = fact
				if !input.Not {
					inputs[name] = fact
				}
			}
		case domain.InputDefinitionSelectAll:
			if facts, err := self.getInputFacts(tx, input.Match.WithoutInputs()); err != nil {
				return false, nil, err
			} else if len(facts) == 0 {
				if !input.Not && !input.Optional {
					return false, nil, nil
				}
			} else {
				inputFacts[name] = facts
				if !input.Not {
					inputs[name] = facts
				}
			}
		default:
			return false, nil, fmt.Errorf("InputDefinitionSelect with unknown value %d", input.Select)
		}
	}

	for name, input := range action.Inputs {
		switch input.Select {
		case domain.InputDefinitionSelectLatest:
			if inputFactEntry, exists := inputFact[name]; exists {
				if match, err := matchFact(input.Match.WithInputs(inputs), inputFactEntry); err != nil {
					return false, inputs, err
				} else if match == input.Not {
					if !input.Optional || input.Not {
						return false, inputs, nil
					}
					delete(inputs, name)
				}
			}
		case domain.InputDefinitionSelectAll:
			if inputFactsEntry, exists := inputFacts[name]; exists {
				for i, fact := range inputFactsEntry {
					if match, err := matchFact(input.Match.WithInputs(inputs), fact); err != nil {
						return false, inputs, err
					} else if match == input.Not {
						if !input.Optional || input.Not {
							return false, inputs, nil
						}
						if facts, exists := inputs[name]; exists {
							// We will filter `nil`s out later as doing that here would be costly.
							facts.([]*domain.Fact)[i] = nil
						}
					}
				}

				if facts, exists := inputs[name]; exists {
					// Filter out `nil` entries from non-matching facts.
					newFacts := []*domain.Fact{}
					for _, fact := range facts.([]*domain.Fact) {
						if fact == nil {
							continue
						}
						newFacts = append(newFacts, fact)
					}
					inputs[name] = newFacts

					if len(newFacts) == 0 {
						if !input.Optional {
							return false, inputs, nil
						}
						delete(inputs, name)
					}
				}
			}
		default:
			panic("This should have already been caught by the loop above!")
		}
	}

	for name, input := range action.Inputs {
		switch input.Select {
		case domain.InputDefinitionSelectLatest:
			if entry, exists := inputs[name]; exists {
				filterFields(&entry.(*domain.Fact).Value, input.Match.WithoutInputs())
			}
		case domain.InputDefinitionSelectAll:
			if entry, exists := inputs[name]; exists {
				match := input.Match.WithoutInputs()
				for _, fact := range entry.([]*domain.Fact) {
					filterFields(&fact.Value, match)
				}
			}
		default:
			panic("This should have already been caught by the loop above!")
		}
	}

	return true, inputs, nil
}

func filterFields(factValue *interface{}, filter cue.Value) {
	if strukt, err := filter.Struct(); err != nil {
		if _, factIsMap := (*factValue).(map[string]interface{}); factIsMap {
			// fact is not allowed to be map because filter is not a struct
			*factValue = nil
		}
	} else if value, factIsMap := (*factValue).(map[string]interface{}); factIsMap {
		for k, v := range value {
			if field, err := strukt.FieldByName(k, false); err != nil {
				delete(value, k)
			} else {
				filterFields(&v, field.Value)
				value[k] = v
			}
		}
	} else {
		// fact must be a map because filter is struct
		*factValue = map[string]interface{}{}
	}
}

func (self *actionService) getInputFactLatest(tx pgx.Tx, value cue.Value) (*domain.Fact, error) {
	fact, err := self.factRepository.GetLatestByFields(tx, collectFieldPaths(value))
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &fact, err
}

func (self *actionService) getInputFacts(tx pgx.Tx, value cue.Value) ([]*domain.Fact, error) {
	facts, err := self.factRepository.GetByFields(tx, collectFieldPaths(value))
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return facts, err
}

func matchFact(match cue.Value, fact *domain.Fact) (bool, error) {
	factCue := match.Context().Encode(fact.Value)
	if err := factCue.Err(); err != nil {
		return false, err
	}

	return match.Subsume(factCue, cue.Final()) == nil, nil
}

func collectFieldPaths(value cue.Value) (paths [][]string) {
	if strukt, err := value.Struct(); err != nil {
		return
	} else {
		iter := strukt.Fields()
		for iter.Next() {
			selector := iter.Selector()

			if iter.IsOptional() || selector.IsDefinition() || selector.PkgPath() != "" || !selector.IsString() {
				continue
			}

			path := []string{iter.Label()}

			if _, err := iter.Value().Struct(); err != nil {
				paths = append(paths, path)
			} else {
				value := iter.Value()
				for _, fieldPath := range collectFieldPaths(value) {
					paths = append(paths, append(path, fieldPath...))
				}
			}
		}
	}
	return
}

func (self *actionService) Create(tx pgx.Tx, source, name string) (*domain.Action, error) {
	action := domain.Action{
		ID:     uuid.New(),
		Name:   name,
		Source: source,
	}

	var actionDef domain.ActionDefinition
	if def, err := self.evaluationService.EvaluateAction(source, name, action.ID); err != nil {
		self.logger.Err(err).Send()
		return nil, err
	} else {
		actionDef = def
	}

	action.Meta = actionDef.Meta
	action.Inputs = actionDef.Inputs

	if err := self.Save(tx, &action); err != nil {
		return nil, err
	}

	return &action, self.Invoke(tx, &action)
}

func (self *actionService) Invoke(tx pgx.Tx, action *domain.Action) error {
	if runnable, inputs, err := self.IsRunnable(tx, action); err != nil {
		return err
	} else if runnable {
		if runDef, err := self.evaluationService.EvaluateRun(action.Source, action.Name, action.ID, inputs); err != nil {
			var evalErr EvaluationError
			if errors.As(err, &evalErr) {
				self.logger.Err(evalErr).Str("source", action.Source).Str("name", action.Name).Msg("Could not evaluate action")
			} else {
				return err
			}
		} else if runDef.IsDecision() {
			if runDef.Outputs.Success != nil {
				if err := self.factRepository.Save(tx, &domain.Fact{Value: runDef.Outputs.Success}, nil); err != nil {
					return errors.WithMessage(err, "Could not publish fact")
				}
			}
		} else {
			run := domain.Run{
				ActionId:   action.ID,
				RunOutputs: runDef.Outputs,
			}

			if err := self.runService.Save(tx, &run); err != nil {
				return errors.WithMessage(err, "Could not insert Run")
			}

			runId := run.NomadJobID.String()
			runDef.Job.ID = &runId

			if response, _, err := self.nomadClient.JobsRegister(runDef.Job, &nomad.WriteOptions{}); err != nil {
				return errors.WithMessage(err, "Failed to run Action")
			} else if len(response.Warnings) > 0 {
				self.logger.Warn().
					Str("nomad-job", runId).
					Str("nomad-evaluation", response.EvalID).
					Str("warnings", response.Warnings).
					Msg("Warnings occured registering Nomad job")
			}
		}
	}

	return nil
}
