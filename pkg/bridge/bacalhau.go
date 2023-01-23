package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/filecoin-project/bacalhau/pkg/job"
	"github.com/filecoin-project/bacalhau/pkg/model"
	"github.com/filecoin-project/bacalhau/pkg/publicapi"
	"github.com/filecoin-project/bacalhau/pkg/system"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

const LilypadJobAnnotation string = "lilypad-job"

func init() {
	err := system.InitConfig()
	if err != nil {
		panic(err)
	}
}

type JobRunner interface {
	Create(ctx context.Context, job ContractSubmittedEvent) (BacalhauJobRunningEvent, error)

	FindCompleted(ctx context.Context, jobs []BacalhauJobRunningEvent) ([]BacalhauJobCompletedEvent, []BacalhauJobFailedEvent)
}

type xRunner struct {
	Client *publicapi.APIClient
}

// Create implements JobRunner
func (r *xRunner) Create(ctx context.Context, e ContractSubmittedEvent) (BacalhauJobRunningEvent, error) {
	job, err := model.NewJobWithSaneProductionDefaults()
	if err != nil {
		return nil, errors.Wrap(err, "error creating Bacalhau job")
	}

	job.Spec = e.Spec()
	job.Spec.Annotations = append(job.Spec.Annotations,
		LilypadJobAnnotation,
		fmt.Sprintf("%s-%s", LilypadJobAnnotation, e.OrderId()), // TODO do some encryption thing here
	)
	job, err = r.Client.Submit(ctx, job, nil)
	if err != nil {
		err = errors.Wrap(err, "error submitting Bacalhau job")
	}

	log.Ctx(ctx).Info().Stringer("id", e.OrderId()).Str("job", job.Metadata.ID).Msg("Created Bacalhau job")
	return e.JobCreated(job), err
}

// FindCompleted implements JobRunner
func (runner *xRunner) FindCompleted(ctx context.Context, jobs []BacalhauJobRunningEvent) ([]BacalhauJobCompletedEvent, []BacalhauJobFailedEvent) {
	log.Ctx(ctx).Debug().Int("jobs", len(jobs)).Msg("Looking at job states")

	completed := make([]BacalhauJobCompletedEvent, 0, len(jobs))
	failed := make([]BacalhauJobFailedEvent, 0, len(jobs))

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	bacjobs, err := runner.Client.List(timeoutCtx, "", []model.IncludedTag{model.IncludedTag(LilypadJobAnnotation)}, nil, 100, false, "created_at", true)
	if err != nil {
		log.Ctx(ctx).Error().Err(err).Send()
		return completed, failed
	}

	for _, j := range jobs {
		ctx := log.Ctx(ctx).With().Stringer("id", j.OrderId()).Str("job", j.JobID()).Logger().WithContext(ctx)

		for _, bacjob := range bacjobs {
			if bacjob.Metadata.ID != j.JobID() {
				continue
			}

			totalShards := job.GetJobTotalExecutionCount(bacjob)
			jobStillRunning := job.WaitForTerminalStates(totalShards)
			jobHasErrors := job.WaitThrowErrors([]model.JobStateType{model.JobStateError})
			jobComplete := job.WaitForJobStates(map[model.JobStateType]int{
				model.JobStateCompleted: totalShards,
			})

			if ok, err := jobStillRunning(bacjob.Status.State); !ok || err != nil {
				log.Ctx(ctx).Debug().Err(err).Msg("Bacalhau job still in progress")
			} else if ok, err := jobComplete(bacjob.Status.State); ok && err == nil {
				log.Ctx(ctx).Info().Err(err).Msg("Bacalhau job completed")
				completed = append(completed, j.Completed())
			} else if ok, err := jobHasErrors(bacjob.Status.State); !ok || err != nil {
				log.Ctx(ctx).Info().Err(err).Msg("Bacalhau job failed")
				failed = append(failed, j.Failed())
			} else {
				log.Ctx(ctx).Warn().Msg("Bacalhau job in unknown state")
			}

			break
		}
	}

	return completed, failed
}

var _ JobRunner = (*xRunner)(nil)

func NewJobRunner() JobRunner {
	apiPort := 1234
	apiHost := "bootstrap.production.bacalhau.org"
	client := publicapi.NewAPIClient(fmt.Sprintf("http://%s:%d", apiHost, apiPort))
	return &xRunner{Client: client}
}

type RunnerCreateHandler func(context.Context, ContractSubmittedEvent) (BacalhauJobRunningEvent, error)
type RunnerFindCompletedHandler func(context.Context, []BacalhauJobRunningEvent) ([]BacalhauJobCompletedEvent, []BacalhauJobFailedEvent)

var SuccessfulCreate RunnerCreateHandler = func(ctx context.Context, cse ContractSubmittedEvent) (BacalhauJobRunningEvent, error) {
	return cse.JobCreated(model.NewJob()), nil
}

var ErrorCreate RunnerCreateHandler = func(ctx context.Context, cse ContractSubmittedEvent) (BacalhauJobRunningEvent, error) {
	return nil, errors.New("error creating job")
}

var SuccssfulFind RunnerFindCompletedHandler = func(ctx context.Context, jobs []BacalhauJobRunningEvent) ([]BacalhauJobCompletedEvent, []BacalhauJobFailedEvent) {
	completed := []BacalhauJobCompletedEvent{}
	for _, job := range jobs {
		completed = append(completed, job.(BacalhauJobCompletedEvent))
	}
	return completed, nil
}

var FailedFind RunnerFindCompletedHandler = func(ctx context.Context, jobs []BacalhauJobRunningEvent) ([]BacalhauJobCompletedEvent, []BacalhauJobFailedEvent) {
	failed := []BacalhauJobFailedEvent{}
	for _, job := range jobs {
		failed = append(failed, job.(BacalhauJobFailedEvent))
	}
	return nil, failed
}

type mockRunner struct {
	CreateHandler        RunnerCreateHandler
	FindCompletedHandler RunnerFindCompletedHandler
}

// Create implements JobRunner
func (mock *mockRunner) Create(ctx context.Context, job ContractSubmittedEvent) (BacalhauJobRunningEvent, error) {
	if mock.CreateHandler != nil {
		return mock.CreateHandler(ctx, job)
	} else {
		return SuccessfulCreate(ctx, job)
	}
}

// FindCompleted implements JobRunner
func (mock *mockRunner) FindCompleted(ctx context.Context, jobs []BacalhauJobRunningEvent) ([]BacalhauJobCompletedEvent, []BacalhauJobFailedEvent) {
	if mock.FindCompletedHandler != nil {
		return mock.FindCompletedHandler(ctx, jobs)
	} else {
		return SuccssfulFind(ctx, jobs)
	}
}

var _ JobRunner = (*mockRunner)(nil)
