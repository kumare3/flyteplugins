package k8s

import (
	"context"
	"strconv"
	"strings"

	idlCore "github.com/lyft/flyteidl/gen/pb-go/flyteidl/core"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/core"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/utils"
	"github.com/lyft/flyteplugins/go/tasks/plugins/array"
	"github.com/lyft/flyteplugins/go/tasks/plugins/array/arraystatus"
	arrayCore "github.com/lyft/flyteplugins/go/tasks/plugins/array/core"
	"github.com/lyft/flyteplugins/go/tasks/plugins/array/errorcollector"
	"github.com/lyft/flytestdlib/bitarray"
	errors2 "github.com/lyft/flytestdlib/errors"
	"github.com/lyft/flytestdlib/logger"
	"github.com/lyft/flytestdlib/storage"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sTypes "k8s.io/apimachinery/pkg/types"
)

type Task struct {
	LogLinks         []*idlCore.TaskLog
	State            *arrayCore.State
	NewArrayStatus   *arraystatus.ArrayStatus
	Config           *Config
	ChildIdx         int
	MessageCollector *errorcollector.ErrorMessageCollector
}

type TaskStatus int8

const (
	Success TaskStatus = iota
	Error
	Waiting
	ReturnState
)

func (t Task) Launch(ctx context.Context, tCtx core.TaskExecutionContext, kubeClient core.KubeClient) (TaskStatus, error) {
	podTemplate, _, err := FlyteArrayJobToK8sPodTemplate(ctx, tCtx)
	if err != nil {
		return Error, errors2.Wrapf(ErrBuildPodTemplate, err, "Failed to convert task template to a pod template for a task")
	}

	var args []string
	if len(podTemplate.Spec.Containers) > 0 {
		args = append(podTemplate.Spec.Containers[0].Command, podTemplate.Spec.Containers[0].Args...)
		podTemplate.Spec.Containers[0].Command = []string{}
	}

	indexStr := strconv.Itoa(t.ChildIdx)
	podName := formatSubTaskName(ctx, tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), indexStr)

	pod := podTemplate.DeepCopy()
	pod.Name = podName
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  FlyteK8sArrayIndexVarName,
		Value: indexStr,
	})

	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, arrayJobEnvVars...)
	pod.Spec.Containers[0].Args, err = utils.ReplaceTemplateCommandArgs(ctx, args, arrayJobInputReader{tCtx.InputReader()}, tCtx.OutputWriter())
	if err != nil {
		return Error, errors2.Wrapf(ErrReplaceCmdTemplate, err, "Failed to replace cmd args")
	}

	pod = ApplyPodPolicies(ctx, t.Config, pod)

	succeeded, err := allocateResource(ctx, tCtx, t.Config, podName, t.ChildIdx, t.NewArrayStatus)
	if err != nil {
		return Error, err
	}

	if !succeeded {
		return Waiting, nil
	}

	err = kubeClient.GetClient().Create(ctx, pod)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		if k8serrors.IsForbidden(err) {
			if strings.Contains(err.Error(), "exceeded quota") {
				// TODO: Quota errors are retried forever, it would be good to have support for backoff strategy.
				logger.Infof(ctx, "Failed to launch  job, resource quota exceeded. Err: %v", err)
				t.State = t.State.SetPhase(arrayCore.PhaseWaitingForResources, 0).SetReason("Not enough resources to launch job")
			} else {
				t.State = t.State.SetPhase(arrayCore.PhaseRetryableFailure, 0).SetReason("Failed to launch job.")
			}

			t.State = t.State.SetReason(err.Error())
			return ReturnState, nil
		}

		return Error, errors2.Wrapf(ErrSubmitJob, err, "Failed to submit job.")
	}

	return Success, nil
}

func (t Task) Monitor(ctx context.Context, tCtx core.TaskExecutionContext, kubeClient core.KubeClient, dataStore *storage.DataStore, outputPrefix, baseOutputDataSandbox storage.DataReference) (TaskStatus, error) {
	indexStr := strconv.Itoa(t.ChildIdx)
	podName := formatSubTaskName(ctx, tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), indexStr)
	phaseInfo, err := CheckPodStatus(ctx, kubeClient,
		k8sTypes.NamespacedName{
			Name:      podName,
			Namespace: tCtx.TaskExecutionMetadata().GetNamespace(),
		})
	if err != nil {
		return Error, errors2.Wrapf(ErrCheckPodStatus, err, "Failed to check pod status.")
	}

	if phaseInfo.Info() != nil {
		t.LogLinks = append(t.LogLinks, phaseInfo.Info().Logs...)
	}

	if phaseInfo.Err() != nil {
		t.MessageCollector.Collect(t.ChildIdx, phaseInfo.Err().String())
	}

	actualPhase := phaseInfo.Phase()
	if phaseInfo.Phase().IsSuccess() {
		originalIdx := arrayCore.CalculateOriginalIndex(t.ChildIdx, t.State.GetIndexesToCache())
		actualPhase, err = array.CheckTaskOutput(ctx, dataStore, outputPrefix, baseOutputDataSandbox, t.ChildIdx, originalIdx)
	}

	t.NewArrayStatus.Detailed.SetItem(t.ChildIdx, bitarray.Item(actualPhase))
	t.NewArrayStatus.Summary.Inc(actualPhase)

	return Success, nil
}

func (t Task) Abort() {}

func (t Task) Finalize(ctx context.Context, tCtx core.TaskExecutionContext, kubeClient core.KubeClient) error {
	indexStr := strconv.Itoa(t.ChildIdx)
	podName := formatSubTaskName(ctx, tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), indexStr)
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       PodKind,
			APIVersion: metav1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: tCtx.TaskExecutionMetadata().GetNamespace(),
		},
	}

	err := kubeClient.GetClient().Delete(ctx, pod)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	// Deallocate Resouce
	err = deallocateResource(ctx, tCtx, t.Config, t.ChildIdx)
	if err != nil {
		logger.Errorf(ctx, "Error releasing allocation token [%s] in Finalize [%s]", podName, err)
		return err
	}

	return nil

}

func allocateResource(ctx context.Context, tCtx core.TaskExecutionContext, config *Config, podName string, childIdx int, arrayStatus *arraystatus.ArrayStatus) (bool, error) {
	if !IsResourceConfigSet() {
		return true, nil
	}

	resourceNamespace := core.ResourceNamespace(config.ResourceConfig.PrimaryLabel)
	resourceConstraintSpec := createResourceConstraintsSpec(ctx, tCtx)

	allocationStatus, err := tCtx.ResourceManager().AllocateResource(ctx, resourceNamespace, podName, resourceConstraintSpec)
	if err != nil {
		logger.Errorf(ctx, "Resource manager failed for TaskExecId [%s] token [%s]. error %s",
			tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetID(), podName, err)
		return false, err
	}

	if allocationStatus != core.AllocationStatusGranted {
		arrayStatus.Detailed.SetItem(childIdx, bitarray.Item(core.PhaseWaitingForResources))
		arrayStatus.Summary.Inc(core.PhaseWaitingForResources)
		return false, nil
	}

	logger.Infof(ctx, "Allocation result for [%s] is [%s]", podName, allocationStatus)
	return true, nil
}

func deallocateResource(ctx context.Context, tCtx core.TaskExecutionContext, config *Config, childIdx int) error {
	if !IsResourceConfigSet() {
		return nil
	}
	indexStr := strconv.Itoa((childIdx))
	podName := formatSubTaskName(ctx, tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), indexStr)
	resourceNamespace := core.ResourceNamespace(config.ResourceConfig.PrimaryLabel)

	err := tCtx.ResourceManager().ReleaseResource(ctx, resourceNamespace, podName)
	if err != nil {
		logger.Errorf(ctx, "Error releasing token [%s]. error %s", podName, err)
		return err
	}

	return nil
}

func createResourceConstraintsSpec(ctx context.Context, _ core.TaskExecutionContext) core.ResourceConstraintsSpec {
	constraintsSpec := core.ResourceConstraintsSpec{
		ProjectScopeResourceConstraint:   nil,
		NamespaceScopeResourceConstraint: nil,
	}

	if !IsResourceConfigSet() {
		logger.Infof(ctx, "No Resource config is found. Returning an empty resource constraints spec")
		return constraintsSpec
	}

	constraintsSpec.ProjectScopeResourceConstraint = &core.ResourceConstraint{Value: int64(1)}
	constraintsSpec.NamespaceScopeResourceConstraint = &core.ResourceConstraint{Value: int64(1)}
	logger.Infof(ctx, "Created a resource constraints spec: [%v]", constraintsSpec)

	return constraintsSpec
}