/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package populator implements interfaces that monitor and keep the states of the
// desired_state_of_word in sync with the "ground truth" from informer.
package populator

import (
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corelisters "k8s.io/client-go/listers/core/v1"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/cache"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/util"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	volutil "k8s.io/kubernetes/pkg/volume/util"
)

// DesiredStateOfWorldPopulator periodically verifies that the pods in the
// desired state of the world still exist, if not, it removes them.
// It also loops through the list of active pods and ensures that
// each one exists in the desired state of the world cache
// if it has volumes.
type DesiredStateOfWorldPopulator interface {
	Run(stopCh <-chan struct{})
}

// NewDesiredStateOfWorldPopulator returns a new instance of DesiredStateOfWorldPopulator.
// loopSleepDuration - the amount of time the populator loop sleeps between
//     successive executions
// podManager - the kubelet podManager that is the source of truth for the pods
//     that exist on this host
// desiredStateOfWorld - the cache to populate
func NewDesiredStateOfWorldPopulator(
	loopSleepDuration time.Duration,
	listPodsRetryDuration time.Duration,
	podLister corelisters.PodLister,
	desiredStateOfWorld cache.DesiredStateOfWorld,
	volumePluginMgr *volume.VolumePluginMgr,
	pvcLister corelisters.PersistentVolumeClaimLister,
	pvLister corelisters.PersistentVolumeLister,
	csiMigratedPluginManager csimigration.PluginManager,
	intreeToCSITranslator csimigration.InTreeToCSITranslator) DesiredStateOfWorldPopulator {
	return &desiredStateOfWorldPopulator{
		loopSleepDuration:        loopSleepDuration,
		listPodsRetryDuration:    listPodsRetryDuration,
		podLister:                podLister,
		desiredStateOfWorld:      desiredStateOfWorld,
		volumePluginMgr:          volumePluginMgr,
		pvcLister:                pvcLister,
		pvLister:                 pvLister,
		csiMigratedPluginManager: csiMigratedPluginManager,
		intreeToCSITranslator:    intreeToCSITranslator,
	}
}

type desiredStateOfWorldPopulator struct {
	loopSleepDuration        time.Duration
	podLister                corelisters.PodLister
	desiredStateOfWorld      cache.DesiredStateOfWorld
	volumePluginMgr          *volume.VolumePluginMgr
	pvcLister                corelisters.PersistentVolumeClaimLister
	pvLister                 corelisters.PersistentVolumeLister
	listPodsRetryDuration    time.Duration
	timeOfLastListPods       time.Time
	csiMigratedPluginManager csimigration.PluginManager
	intreeToCSITranslator    csimigration.InTreeToCSITranslator
}

func (dswp *desiredStateOfWorldPopulator) Run(stopCh <-chan struct{}) {
	// 周期性调用populatorLoopFunc 维护desiredStateOfWorld数据
	wait.Until(dswp.populatorLoopFunc(), dswp.loopSleepDuration, stopCh)
}

func (dswp *desiredStateOfWorldPopulator) populatorLoopFunc() func() {
	return func() {
		dswp.findAndRemoveDeletedPods()

		// findAndAddActivePods is called periodically, independently of the main
		// populator loop.
		if time.Since(dswp.timeOfLastListPods) < dswp.listPodsRetryDuration {
			klog.V(5).Infof(
				"Skipping findAndAddActivePods(). Not permitted until %v (listPodsRetryDuration %v).",
				dswp.timeOfLastListPods.Add(dswp.listPodsRetryDuration),
				dswp.listPodsRetryDuration)

			return
		}
		dswp.findAndAddActivePods()
	}
}

// Iterate through all pods in desired state of world, and remove if they no
// longer exist in the informer
func (dswp *desiredStateOfWorldPopulator) findAndRemoveDeletedPods() {
	// pod.uid podToAdd(pod nodeName volume)
	for dswPodUID, dswPodToAdd := range dswp.desiredStateOfWorld.GetPodToAdd() {
		// 获取podKey
		dswPodKey, err := kcache.MetaNamespaceKeyFunc(dswPodToAdd.Pod)
		if err != nil {
			klog.Errorf("MetaNamespaceKeyFunc failed for pod %q (UID %q) with: %v", dswPodKey, dswPodUID, err)
			continue
		}

		// Retrieve the pod object from pod informer with the namespace key
		namespace, name, err := kcache.SplitMetaNamespaceKey(dswPodKey)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("error splitting dswPodKey %q: %v", dswPodKey, err))
			continue
		}
		// 获取pod对象
		informerPod, err := dswp.podLister.Pods(namespace).Get(name)
		switch {
		case errors.IsNotFound(err):
			// pod都找不到了 还是删了把
			// if we can't find the pod, we need to delete it below
		case err != nil:
			klog.Errorf("podLister Get failed for pod %q (UID %q) with %v", dswPodKey, dswPodUID, err)
			continue
		default:
			//
			volumeActionFlag := util.DetermineVolumeAction(
				informerPod,
				dswp.desiredStateOfWorld,
				true /* default volume action */)

			if volumeActionFlag {
				informerPodUID := volutil.GetUniquePodName(informerPod)
				// Check whether the unique identifier of the pod from dsw matches the one retrieved from pod informer
				// 判断uid是否一样
				if informerPodUID == dswPodUID {
					klog.V(10).Infof("Verified pod %q (UID %q) from dsw exists in pod informer.", dswPodKey, dswPodUID)
					continue
				}
			}
		}

		// the pod from dsw does not exist in pod informer, or it does not match the unique identifier retrieved
		// from the informer, delete it from dsw
		// 当前pod的UID和dwsp中Pod的Uid不一致则需要删除在dswp中的pod 很大的可能是在dswp中的pod 在infomer中已经不在了
		klog.V(1).Infof("Removing pod %q (UID %q) from dsw because it does not exist in pod informer.", dswPodKey, dswPodUID)
		dswp.desiredStateOfWorld.DeletePod(dswPodUID, dswPodToAdd.VolumeName, dswPodToAdd.NodeName)
	}
}

func (dswp *desiredStateOfWorldPopulator) findAndAddActivePods() {
	// 遍历所有的Pod 将Pod加入到desiredStateOfWorld中
	pods, err := dswp.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("podLister List failed: %v", err)
		return
	}
	dswp.timeOfLastListPods = time.Now()

	for _, pod := range pods {
		if volutil.IsPodTerminated(pod, pod.Status) {
			// Do not add volumes for terminated pods
			continue
		}
		util.ProcessPodVolumes(pod, true,
			dswp.desiredStateOfWorld, dswp.volumePluginMgr, dswp.pvcLister, dswp.pvLister, dswp.csiMigratedPluginManager, dswp.intreeToCSITranslator)

	}

}
