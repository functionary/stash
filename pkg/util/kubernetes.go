package util

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"github.com/appscode/go/log/golog"
	core_util "github.com/appscode/kutil/core/v1"
	"github.com/appscode/kutil/meta"
	"github.com/appscode/kutil/tools/analytics"
	api "github.com/appscode/stash/apis/stash/v1alpha1"
	stash_listers "github.com/appscode/stash/client/listers/stash/v1alpha1"
	"github.com/appscode/stash/pkg/docker"
	oc "github.com/openshift/client-go/apps/clientset/versioned"
	"github.com/pkg/errors"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

const (
	StashContainer       = "stash"
	LocalVolumeName      = "stash-local"
	ScratchDirVolumeName = "stash-scratchdir"
	PodinfoVolumeName    = "stash-podinfo"

	RecoveryJobPrefix   = "stash-recovery-"
	ScaledownCronPrefix = "stash-scaledown-cron-"
	CheckJobPrefix      = "stash-check-"

	AnnotationRestic     = "restic"
	AnnotationRecovery   = "recovery"
	AnnotationOperation  = "operation"
	AnnotationOldReplica = "old-replica"

	OperationRecovery = "recovery"
	OperationCheck    = "check"

	AppLabelStash      = "stash"
	OperationScaleDown = "scale-down"
)

var (
	AnalyticsClientID string
	EnableAnalytics   = true
	LoggerOptions     golog.Options
)

func GetAppliedRestic(m map[string]string) (*api.Restic, error) {
	data := GetString(m, api.LastAppliedConfiguration)
	if data == "" {
		return nil, nil
	}
	obj, err := meta.UnmarshalFromJSON([]byte(data), api.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}
	restic, ok := obj.(*api.Restic)
	if !ok {
		return nil, fmt.Errorf("%s annotations has invalid Rectic object", api.LastAppliedConfiguration)
	}
	return restic, nil
}

func FindRestic(lister stash_listers.ResticLister, obj metav1.ObjectMeta) (*api.Restic, error) {
	restics, err := lister.Restics(obj.Namespace).List(labels.Everything())
	if kerr.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	result := make([]*api.Restic, 0)
	for _, restic := range restics {
		selector, err := metav1.LabelSelectorAsSelector(&restic.Spec.Selector)
		if err != nil {
			return nil, err
		}
		if selector.Matches(labels.Set(obj.Labels)) {
			result = append(result, restic)
		}
	}
	if len(result) > 1 {
		var msg bytes.Buffer
		msg.WriteString(fmt.Sprintf("Workload %s/%s matches multiple Restics:", obj.Namespace, obj.Name))
		for i, restic := range result {
			if i > 0 {
				msg.WriteString(", ")
			}
			msg.WriteString(restic.Name)
		}
		return nil, errors.New(msg.String())
	} else if len(result) == 1 {
		return result[0], nil
	}
	return nil, nil
}

func GetString(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

func PushgatewayURL() string {
	// called by operator, returning its own namespace. Since pushgateway runs as a side-car with operator, this works!
	return fmt.Sprintf("http://stash-operator.%s.svc:56789", meta.Namespace())
}

func NewInitContainer(r *api.Restic, workload api.LocalTypedReference, image docker.Docker, enableRBAC bool) core.Container {
	container := NewSidecarContainer(r, workload, image)
	container.Args = []string{
		"backup",
		"--restic-name=" + r.Name,
		"--workload-kind=" + workload.Kind,
		"--workload-name=" + workload.Name,
		"--docker-registry=" + image.Registry,
		"--image-tag=" + image.Tag,
		"--pushgateway-url=" + PushgatewayURL(),
		fmt.Sprintf("--enable-analytics=%v", EnableAnalytics),
	}
	container.Args = append(container.Args, LoggerOptions.ToFlags()...)
	if enableRBAC {
		container.Args = append(container.Args, "--enable-rbac=true")
	}

	return container
}

func NewSidecarContainer(r *api.Restic, workload api.LocalTypedReference, image docker.Docker) core.Container {
	if r.Annotations != nil {
		if v, ok := r.Annotations[api.VersionTag]; ok {
			image.Tag = v
		}
	}
	sidecar := core.Container{
		Name:  StashContainer,
		Image: image.ToContainerImage(),
		Args: append([]string{
			"backup",
			"--restic-name=" + r.Name,
			"--workload-kind=" + workload.Kind,
			"--workload-name=" + workload.Name,
			"--docker-registry=" + image.Registry,
			"--image-tag=" + image.Tag,
			"--run-via-cron=true",
			"--pushgateway-url=" + PushgatewayURL(),
			fmt.Sprintf("--enable-analytics=%v", EnableAnalytics),
		}, LoggerOptions.ToFlags()...),
		Env: []core.EnvVar{
			{
				Name: "NODE_NAME",
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{
						FieldPath: "spec.nodeName",
					},
				},
			},
			{
				Name: "POD_NAME",
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{
						FieldPath: "metadata.name",
					},
				},
			},
			{
				Name:  analytics.Key,
				Value: AnalyticsClientID,
			},
		},
		Resources: r.Spec.Resources,
		VolumeMounts: []core.VolumeMount{
			{
				Name:      ScratchDirVolumeName,
				MountPath: "/tmp",
			},
			{
				Name:      PodinfoVolumeName,
				MountPath: "/etc/stash",
			},
		},
	}
	for _, srcVol := range r.Spec.VolumeMounts {
		sidecar.VolumeMounts = append(sidecar.VolumeMounts, core.VolumeMount{
			Name:      srcVol.Name,
			MountPath: srcVol.MountPath,
			SubPath:   srcVol.SubPath,
			ReadOnly:  true,
		})
	}
	if r.Spec.Backend.Local != nil {
		_, mnt := r.Spec.Backend.Local.ToVolumeAndMount(LocalVolumeName)
		sidecar.VolumeMounts = append(sidecar.VolumeMounts, mnt)
	}
	return sidecar
}

func UpsertScratchVolume(volumes []core.Volume) []core.Volume {
	return core_util.UpsertVolume(volumes, core.Volume{
		Name: ScratchDirVolumeName,
		VolumeSource: core.VolumeSource{
			EmptyDir: &core.EmptyDirVolumeSource{},
		},
	})
}

// https://kubernetes.io/docs/tasks/inject-data-application/downward-api-volume-expose-pod-information/#store-pod-fields
func UpsertDownwardVolume(volumes []core.Volume) []core.Volume {
	return core_util.UpsertVolume(volumes, core.Volume{
		Name: PodinfoVolumeName,
		VolumeSource: core.VolumeSource{
			DownwardAPI: &core.DownwardAPIVolumeSource{
				Items: []core.DownwardAPIVolumeFile{
					{
						Path: "labels",
						FieldRef: &core.ObjectFieldSelector{
							FieldPath: "metadata.labels",
						},
					},
				},
			},
		},
	})
}

func MergeLocalVolume(volumes []core.Volume, old, new *api.Restic) []core.Volume {
	oldPos := -1
	if old != nil && old.Spec.Backend.Local != nil {
		for i, vol := range volumes {
			if vol.Name == LocalVolumeName {
				oldPos = i
				break
			}
		}
	}
	if new.Spec.Backend.Local != nil {
		vol, _ := new.Spec.Backend.Local.ToVolumeAndMount(LocalVolumeName)
		if oldPos != -1 {
			volumes[oldPos] = vol
		} else {
			volumes = core_util.UpsertVolume(volumes, vol)
		}
	} else {
		if oldPos != -1 {
			volumes = append(volumes[:oldPos], volumes[oldPos+1:]...)
		}
	}
	return volumes
}

func EnsureVolumeDeleted(volumes []core.Volume, name string) []core.Volume {
	for i, v := range volumes {
		if v.Name == name {
			return append(volumes[:i], volumes[i+1:]...)
		}
	}
	return volumes
}

func ResticEqual(old, new *api.Restic) bool {
	var oldSpec, newSpec *api.ResticSpec
	if old != nil {
		oldSpec = &old.Spec
	}
	if new != nil {
		newSpec = &new.Spec
	}
	return meta.Equal(oldSpec, newSpec)
}

func RecoveryEqual(old, new *api.Recovery) bool {
	var oldSpec, newSpec *api.RecoverySpec
	if old != nil {
		oldSpec = &old.Spec
	}
	if new != nil {
		newSpec = &new.Spec
	}
	return reflect.DeepEqual(oldSpec, newSpec)
}

func NewRecoveryJob(recovery *api.Recovery, image docker.Docker) *batch.Job {
	volumes := make([]core.Volume, 0)
	volumeMounts := make([]core.VolumeMount, 0)
	for i, recVol := range recovery.Spec.RecoveredVolumes {
		vol, mnt := recVol.ToVolumeAndMount(fmt.Sprintf("vol-%d", i))
		volumes = append(volumes, vol)
		volumeMounts = append(volumeMounts, mnt)
	}

	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RecoveryJobPrefix + recovery.Name,
			Namespace: recovery.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: api.SchemeGroupVersion.String(),
					Kind:       api.ResourceKindRecovery,
					Name:       recovery.Name,
					UID:        recovery.UID,
				},
			},
			Labels: map[string]string{
				"app":               AppLabelStash,
				AnnotationRecovery:  recovery.Name,
				AnnotationOperation: OperationRecovery,
			},
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				Spec: core.PodSpec{
					Containers: []core.Container{
						{
							Name:  StashContainer,
							Image: image.ToContainerImage(),
							Args: append([]string{
								"recover",
								"--recovery-name=" + recovery.Name,
								fmt.Sprintf("--enable-analytics=%v", EnableAnalytics),
							}, LoggerOptions.ToFlags()...),
							Env: []core.EnvVar{
								{
									Name:  analytics.Key,
									Value: AnalyticsClientID,
								},
							},
							VolumeMounts: append(volumeMounts, core.VolumeMount{
								Name:      ScratchDirVolumeName,
								MountPath: "/tmp",
							}),
						},
					},
					ImagePullSecrets: recovery.Spec.ImagePullSecrets,
					RestartPolicy:    core.RestartPolicyOnFailure,
					Volumes: append(volumes, core.Volume{
						Name: ScratchDirVolumeName,
						VolumeSource: core.VolumeSource{
							EmptyDir: &core.EmptyDirVolumeSource{},
						},
					}),
					NodeName: recovery.Spec.NodeName,
				},
			},
		},
	}

	// local backend
	if recovery.Spec.Backend.Local != nil {
		vol, mnt := recovery.Spec.Backend.Local.ToVolumeAndMount(LocalVolumeName)
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts, mnt)
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, vol)
	}

	return job
}

func WorkloadExists(kc kubernetes.Interface, occ oc.Interface, namespace string, workload api.LocalTypedReference) error {
	if err := workload.Canonicalize(); err != nil {
		return err
	}

	switch workload.Kind {
	case api.KindDeployment:
		_, err := kc.AppsV1beta1().Deployments(namespace).Get(workload.Name, metav1.GetOptions{})
		return err
	case api.KindReplicaSet:
		_, err := kc.ExtensionsV1beta1().ReplicaSets(namespace).Get(workload.Name, metav1.GetOptions{})
		return err
	case api.KindReplicationController:
		_, err := kc.CoreV1().ReplicationControllers(namespace).Get(workload.Name, metav1.GetOptions{})
		return err
	case api.KindStatefulSet:
		_, err := kc.AppsV1beta1().StatefulSets(namespace).Get(workload.Name, metav1.GetOptions{})
		return err
	case api.KindDaemonSet:
		_, err := kc.ExtensionsV1beta1().DaemonSets(namespace).Get(workload.Name, metav1.GetOptions{})
		return err
	case api.KindDeploymentConfig:
		_, err := occ.AppsV1().DeploymentConfigs(namespace).Get(workload.Name, metav1.GetOptions{})
		return err
	default:
		fmt.Errorf(`unrecognized workload "Kind" %v`, workload.Kind)
	}
	return nil
}

func GetConfigmapLockName(workload api.LocalTypedReference) string {
	return strings.ToLower(fmt.Sprintf("lock-%s-%s", workload.Kind, workload.Name))
}

func DeleteConfigmapLock(k8sClient kubernetes.Interface, namespace string, workload api.LocalTypedReference) error {
	return k8sClient.CoreV1().ConfigMaps(namespace).Delete(GetConfigmapLockName(workload), &metav1.DeleteOptions{})
}

func NewCheckJob(restic *api.Restic, hostName, smartPrefix string, image docker.Docker) *batch.Job {
	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CheckJobPrefix + restic.Name,
			Namespace: restic.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: api.SchemeGroupVersion.String(),
					Kind:       api.ResourceKindRestic,
					Name:       restic.Name,
					UID:        restic.UID,
				},
			},
			Labels: map[string]string{
				"app":               AppLabelStash,
				AnnotationRestic:    restic.Name,
				AnnotationOperation: OperationCheck,
			},
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				Spec: core.PodSpec{
					Containers: []core.Container{
						{
							Name:  StashContainer,
							Image: image.ToContainerImage(),
							Args: append([]string{
								"check",
								"--restic-name=" + restic.Name,
								"--host-name=" + hostName,
								"--smart-prefix=" + smartPrefix,
								fmt.Sprintf("--enable-analytics=%v", EnableAnalytics),
							}, LoggerOptions.ToFlags()...),
							Env: []core.EnvVar{
								{
									Name:  analytics.Key,
									Value: AnalyticsClientID,
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      ScratchDirVolumeName,
									MountPath: "/tmp",
								},
							},
						},
					},
					ImagePullSecrets: restic.Spec.ImagePullSecrets,
					RestartPolicy:    core.RestartPolicyOnFailure,
					Volumes: []core.Volume{
						{
							Name: ScratchDirVolumeName,
							VolumeSource: core.VolumeSource{
								EmptyDir: &core.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	// local backend
	// user don't need to specify "stash-local" volume, we collect it from restic-spec
	if restic.Spec.Backend.Local != nil {
		vol, mnt := restic.Spec.Backend.Local.ToVolumeAndMount(LocalVolumeName)
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts, mnt)
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, vol)
	}

	return job
}

func WorkloadReplicas(kc *kubernetes.Clientset, occ oc.Interface, namespace string, workloadKind string, workloadName string) (int32, error) {
	switch workloadKind {
	case api.KindDeployment:
		obj, err := kc.AppsV1beta1().Deployments(namespace).Get(workloadName, metav1.GetOptions{})
		if err != nil {
			return 0, err
		} else {
			return *obj.Spec.Replicas, nil
		}
	case api.KindReplicationController:
		obj, err := kc.CoreV1().ReplicationControllers(namespace).Get(workloadName, metav1.GetOptions{})
		if err != nil {
			return 0, err
		} else {
			return *obj.Spec.Replicas, nil
		}
	case api.KindReplicaSet:
		obj, err := kc.ExtensionsV1beta1().ReplicaSets(namespace).Get(workloadName, metav1.GetOptions{})
		if err != nil {
			return 0, err
		} else {
			return *obj.Spec.Replicas, nil
		}
	case api.KindDeploymentConfig:
		obj, err := occ.AppsV1().DeploymentConfigs(namespace).Get(workloadName, metav1.GetOptions{})
		if err != nil {
			return 0, err
		} else {
			return obj.Spec.Replicas, nil
		}
	default:
		return 0, fmt.Errorf("unknown workload type")
	}
	return 0, nil
}
