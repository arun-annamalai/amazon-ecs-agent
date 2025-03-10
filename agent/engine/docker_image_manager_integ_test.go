//go:build integration
// +build integration

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// The DockerTaskEngine is an abstraction over the DockerGoClient so that
// it does not have to know about tasks, only containers

package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/agent/api/container/status"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	apitaskstatus "github.com/aws/amazon-ecs-agent/agent/api/task/status"
	"github.com/aws/amazon-ecs-agent/agent/data"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient/sdkclientfactory"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	imageRemovalTimeout       = 30 * time.Second
	taskCleanupTimeoutSeconds = 30
)

// Deletion of images in the order of LRU time: Happy path
//
//	a. This includes starting up agent, pull images, start containers,
//	  account them in image manager,  stop containers, remove containers, account this in image manager,
//	b. Simulate the pulled time (so that it passes the minimum age criteria
//	  for getting chosen for deletion )
//	c. Start image cleanup , ensure that ONLY the top 2 eligible LRU images
//	  are removed from the instance,  and those deleted images’ image states are removed from image manager.
//	d. Ensure images that do not pass the ‘minimumAgeForDeletion’ criteria are not removed.
//	e. Image has not passed the ‘hasNoAssociatedContainers’ criteria.
//	f. Ensure that that if not eligible, image is not deleted from the instance and image reference in ImageManager is not removed.
func TestIntegImageCleanupHappyCase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`Skipping this test because of error: level=error time=2020-05-27T20:20:03Z msg="Error removing` +
			` Image amazon/image-cleanup-test-image2:make - Error response from daemon: hcsshim::GetComputeSystems:` +
			` The requested compute system operation is not valid in the current state." module=log.go`)
	}
	cfg := defaultTestConfigIntegTest()
	cfg.TaskCleanupWaitDuration = 5 * time.Second

	// Set low values so this test can complete in a sane amout of time
	cfg.MinimumImageDeletionAge = 1 * time.Second
	cfg.NumImagesToDeletePerCycle = 2
	// start agent
	taskEngine, done, _ := setup(cfg, nil, t)

	imageManager := taskEngine.(*DockerTaskEngine).imageManager.(*dockerImageManager)
	imageManager.SetDataClient(data.NewNoopClient())

	defer func() {
		done()
		// Force cleanup all test images and containers
		cleanupImagesHappy(imageManager)
	}()

	stateChangeEvents := taskEngine.StateChangeEvents()

	// Create test Task
	taskName := "imgClean"
	testTask := createImageCleanupHappyTestTask(taskName)

	go taskEngine.AddTask(testTask)

	// Verify that Task is running
	err := verifyTaskIsRunning(stateChangeEvents, testTask)
	if err != nil {
		t.Fatal(err)
	}

	imageState1, ok := imageManager.GetImageStateFromImageName(test1Image1Name)
	require.True(t, ok, "Could not find image state for %s", test1Image1Name)
	t.Logf("Found image state for %s", test1Image1Name)

	imageState2, ok := imageManager.GetImageStateFromImageName(test1Image2Name)
	require.True(t, ok, "Could not find image state for %s", test1Image2Name)
	t.Logf("Found image state for %s", test1Image2Name)

	imageState3, ok := imageManager.GetImageStateFromImageName(test1Image3Name)
	require.True(t, ok, "Could not find image state for %s", test1Image3Name)
	t.Logf("Found image state for %s", test1Image3Name)

	imageState1ImageID := imageState1.Image.ImageID
	imageState2ImageID := imageState2.Image.ImageID
	imageState3ImageID := imageState3.Image.ImageID

	// Set the ImageState.LastUsedAt to a value far in the past to ensure the test images are deleted.
	// This will make these test images the LRU images.
	imageState1.LastUsedAt = imageState1.LastUsedAt.Add(-99995 * time.Hour)
	imageState2.LastUsedAt = imageState2.LastUsedAt.Add(-99994 * time.Hour)
	imageState3.LastUsedAt = imageState3.LastUsedAt.Add(-99993 * time.Hour)

	// Verify Task is stopped.
	verifyTaskIsStopped(stateChangeEvents, testTask)
	testTask.SetSentStatus(apitaskstatus.TaskStopped)

	// Allow Task cleanup to occur
	time.Sleep(5 * time.Second)

	// Verify Task is cleaned up
	err = verifyTaskIsCleanedUp(taskName, taskEngine)
	if err != nil {
		t.Fatal(err)
	}

	// Call Image removal
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	imageManager.removeUnusedImages(ctx)

	// Verify top 2 LRU images are deleted from image manager
	err = verifyImagesAreRemoved(imageManager, imageState1ImageID, imageState2ImageID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify 3rd LRU image is not removed
	err = verifyImagesAreNotRemoved(imageManager, imageState3ImageID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify top 2 LRU images are removed from docker
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageState1ImageID)
	if !client.IsErrNotFound(err) {
		t.Fatalf("Image was not removed successfully")
	}
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageState2ImageID)
	if !client.IsErrNotFound(err) {
		t.Fatalf("Image was not removed successfully")
	}

	// Verify 3rd LRU image has not been removed from Docker
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageState3ImageID)
	if err != nil {
		t.Fatalf("Image should not have been removed from Docker")
	}
}

// Test that images not falling in the image deletion eligibility criteria are not removed:
//
//	a. Ensure images that do not pass the ‘minimumAgeForDeletion’ criteria are not removed.
//	b. Image has not passed the ‘hasNoAssociatedContainers’ criteria.
//	c. Ensure that the image is not deleted from the instance and image reference in ImageManager is not removed.
func TestIntegImageCleanupThreshold(t *testing.T) {
	cfg := defaultTestConfigIntegTest()
	cfg.TaskCleanupWaitDuration = 1 * time.Second

	// Set low values so this test can complete in a sane amout of time
	cfg.MinimumImageDeletionAge = 15 * time.Minute
	// Set to delete three images, but in this test we expect only two images to be removed
	cfg.NumImagesToDeletePerCycle = 3
	// start agent
	taskEngine, done, _ := setup(cfg, nil, t)

	imageManager := taskEngine.(*DockerTaskEngine).imageManager.(*dockerImageManager)
	imageManager.SetDataClient(data.NewNoopClient())

	defer func() {
		done()
		// Force cleanup all test images and containers
		cleanupImagesThreshold(imageManager)
	}()

	stateChangeEvents := taskEngine.StateChangeEvents()

	// Create test Task
	taskName := "imgClean"
	testTask := createImageCleanupThresholdTestTask(taskName)

	// Start Task
	go taskEngine.AddTask(testTask)

	// Verify that Task is running
	err := verifyTaskIsRunning(stateChangeEvents, testTask)
	if err != nil {
		t.Fatal(err)
	}

	imageState1, ok := imageManager.GetImageStateFromImageName(test2Image1Name)
	require.True(t, ok, "Could not find image state for %s", test2Image1Name)
	t.Logf("Found image state for %s", test2Image1Name)

	imageState2, ok := imageManager.GetImageStateFromImageName(test2Image2Name)
	require.True(t, ok, "Could not find image state for %s", test2Image2Name)
	t.Logf("Found image state for %s", test2Image2Name)

	imageState3, ok := imageManager.GetImageStateFromImageName(test2Image3Name)
	require.True(t, ok, "Could not find image state for %s", test2Image3Name)
	t.Logf("Found image state for %s", test2Image3Name)

	imageState1ImageID := imageState1.Image.ImageID
	imageState2ImageID := imageState2.Image.ImageID
	imageState3ImageID := imageState3.Image.ImageID

	// Set the ImageState.LastUsedAt to a value far in the past to ensure the test images are deleted.
	// This will make these the LRU images so they are deleted.
	imageState1.LastUsedAt = imageState1.LastUsedAt.Add(-99995 * time.Hour)
	imageState2.LastUsedAt = imageState2.LastUsedAt.Add(-99994 * time.Hour)
	imageState3.LastUsedAt = imageState3.LastUsedAt.Add(-99993 * time.Hour)

	// Set two containers to have pull time > threshold
	imageState1.PulledAt = imageState1.PulledAt.Add(-20 * time.Minute)
	imageState2.PulledAt = imageState2.PulledAt.Add(-10 * time.Minute)
	imageState3.PulledAt = imageState3.PulledAt.Add(-25 * time.Minute)

	// Verify Task is stopped
	verifyTaskIsStopped(stateChangeEvents, testTask)
	testTask.SetSentStatus(apitaskstatus.TaskStopped)

	// Allow Task cleanup to occur
	time.Sleep(5 * time.Second)

	// Verify Task is cleaned up
	err = verifyTaskIsCleanedUp(taskName, taskEngine)
	if err != nil {
		t.Fatal(err)
	}

	// Call Image removal
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	imageManager.removeUnusedImages(ctx)

	// Verify Image1 & Image3 are removed from ImageManager as they are beyond the minimumAge threshold
	err = verifyImagesAreRemoved(imageManager, imageState1ImageID, imageState3ImageID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify Image2 is not removed, below threshold for minimumAge
	err = verifyImagesAreNotRemoved(imageManager, imageState2ImageID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify Image1 & Image3 are removed from docker
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageState1ImageID)
	if !client.IsErrNotFound(err) {
		t.Fatalf("Image was not removed successfully")
	}
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageState3ImageID)
	if !client.IsErrNotFound(err) {
		t.Fatalf("Image was not removed successfully")
	}

	// Verify Image2 has not been removed from Docker
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageState2ImageID)
	if err != nil {
		t.Fatalf("Image should not have been removed from Docker")
	}
}

// TestImageWithSameNameAndDifferentID tests image can be correctly removed when tasks
// are running with the same image name, but different image id.
func TestImageWithSameNameAndDifferentID(t *testing.T) {
	cfg := defaultTestConfigIntegTest()
	cfg.TaskCleanupWaitDuration = 1 * time.Second
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	// Set low values so this test can complete in a sane amout of time
	cfg.MinimumImageDeletionAge = 15 * time.Minute

	taskEngine, done, _ := setup(cfg, nil, t)
	defer done()

	dockerClient := taskEngine.(*DockerTaskEngine).client

	// DockerClient doesn't implement TagImage, create a go docker client
	sdkDockerClient, err := client.NewClientWithOpts(client.WithVersion(sdkclientfactory.GetDefaultVersion().String()))
	require.NoError(t, err, "Creating SDK docker client failed")

	imageManager := taskEngine.(*DockerTaskEngine).imageManager.(*dockerImageManager)
	imageManager.SetDataClient(data.NewNoopClient())

	stateChangeEvents := taskEngine.StateChangeEvents()

	// Pull the images needed for the test
	if _, err = dockerClient.InspectImage(test3Image1Name); client.IsErrNotFound(err) {
		metadata := dockerClient.PullImage(ctx, test3Image1Name, nil, dockerclient.LoadImageTimeout)
		assert.NoError(t, metadata.Error, "Failed to pull image %s", test3Image1Name)
	}
	if _, err = dockerClient.InspectImage(test3Image2Name); client.IsErrNotFound(err) {
		metadata := dockerClient.PullImage(ctx, test3Image2Name, nil, dockerclient.LoadImageTimeout)
		assert.NoError(t, metadata.Error, "Failed to pull image %s", test3Image2Name)
	}
	if _, err = dockerClient.InspectImage(test3Image3Name); client.IsErrNotFound(err) {
		metadata := dockerClient.PullImage(ctx, test3Image3Name, nil, dockerclient.LoadImageTimeout)
		assert.NoError(t, metadata.Error, "Failed to pull image %s", test3Image3Name)
	}

	// The same image name used by all tasks in this test
	identicalImageName := "testimagewithsamenameanddifferentid:latest"
	// Create three tasks which use the image with same name but different ID
	task1 := createTestTask("task1")
	task2 := createTestTask("task2")
	task3 := createTestTask("task3")
	task1.Containers[0].Image = identicalImageName
	task2.Containers[0].Image = identicalImageName
	task3.Containers[0].Image = identicalImageName

	err = renameImage(test3Image1Name, task1.Containers[0].Image, sdkDockerClient)
	assert.NoError(t, err, "Renaming the image failed")

	// start and wait for task1 to be running
	go taskEngine.AddTask(task1)
	err = verifyTaskIsRunning(stateChangeEvents, task1)
	require.NoError(t, err, "task1")

	// Verify image state is updated correctly
	imageState1, ok := imageManager.GetImageStateFromImageName(identicalImageName)
	require.True(t, ok, "Could not find image state for %s", identicalImageName)
	t.Logf("Found image state for %s", identicalImageName)
	imageID1 := imageState1.Image.ImageID

	// Using another image but rename to the same name as task1 for task2
	err = renameImage(test3Image2Name, task2.Containers[0].Image, sdkDockerClient)
	require.NoError(t, err, "Renaming the image failed")

	// Start and wait for task2 to be running
	go taskEngine.AddTask(task2)
	err = verifyTaskIsRunning(stateChangeEvents, task2)
	require.NoError(t, err, "task2")

	// Verify image state is updated correctly
	imageState2, ok := imageManager.GetImageStateFromImageName(identicalImageName)
	require.True(t, ok, "Could not find image state for %s", identicalImageName)
	t.Logf("Found image state for %s", identicalImageName)
	imageID2 := imageState2.Image.ImageID
	require.NotEqual(t, imageID2, imageID1, "The image id in task 2 should be different from image in task 1")

	// Using a different image for task3 and rename it to the same name as task1 and task2
	err = renameImage(test3Image3Name, task3.Containers[0].Image, sdkDockerClient)
	require.NoError(t, err, "Renaming the image failed")

	// Start and wait for task3 to be running
	go taskEngine.AddTask(task3)
	err = verifyTaskIsRunning(stateChangeEvents, task3)
	require.NoError(t, err, "task3")

	// Verify image state is updated correctly
	imageState3, ok := imageManager.GetImageStateFromImageName(identicalImageName)
	require.True(t, ok, "Could not find image state for %s", identicalImageName)
	t.Logf("Found image state for %s", identicalImageName)
	imageID3 := imageState3.Image.ImageID
	require.NotEqual(t, imageID3, imageID1, "The image id in task3 should be different from image in task1")
	require.NotEqual(t, imageID3, imageID2, "The image id in task3 should be different from image in task2")

	// Modify image state sothat the image is eligible for deletion
	imageState1.LastUsedAt = imageState1.LastUsedAt.Add(-99995 * time.Hour)
	imageState2.LastUsedAt = imageState2.LastUsedAt.Add(-99994 * time.Hour)
	imageState3.LastUsedAt = imageState3.LastUsedAt.Add(-99993 * time.Hour)

	imageState1.PulledAt = imageState1.PulledAt.Add(-20 * time.Minute)
	imageState2.PulledAt = imageState2.PulledAt.Add(-19 * time.Minute)
	imageState3.PulledAt = imageState3.PulledAt.Add(-18 * time.Minute)

	go discardEvents(stateChangeEvents)
	// Wait for task to be stopped
	waitForTaskStoppedByCheckStatus(task1)
	waitForTaskStoppedByCheckStatus(task2)
	waitForTaskStoppedByCheckStatus(task3)

	task1.SetSentStatus(apitaskstatus.TaskStopped)
	task2.SetSentStatus(apitaskstatus.TaskStopped)
	task3.SetSentStatus(apitaskstatus.TaskStopped)

	// Allow Task cleanup to occur
	time.Sleep(5 * time.Second)

	err = verifyTaskIsCleanedUp("task1", taskEngine)
	assert.NoError(t, err, "task1")
	err = verifyTaskIsCleanedUp("task2", taskEngine)
	assert.NoError(t, err, "task2")
	err = verifyTaskIsCleanedUp("task3", taskEngine)
	assert.NoError(t, err, "task3")

	imageManager.removeUnusedImages(ctx)

	// Verify all the three images are removed from image manager
	err = verifyImagesAreRemoved(imageManager, imageID1, imageID2, imageID3)
	require.NoError(t, err)

	// Verify images are removed by docker
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageID1)
	assert.True(t, client.IsErrNotFound(err), "Image was not removed successfully, image: %s", imageID1)
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageID2)
	assert.True(t, client.IsErrNotFound(err), "Image was not removed successfully, image: %s", imageID2)
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageID3)
	assert.True(t, client.IsErrNotFound(err), "Image was not removed successfully, image: %s", imageID3)
}

// TestImageWithSameIDAndDifferentNames tests images can be correctly removed if
// tasks are running with the same image id but different image name
func TestImageWithSameIDAndDifferentNames(t *testing.T) {
	cfg := defaultTestConfigIntegTest()
	cfg.TaskCleanupWaitDuration = 1 * time.Second
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	// Set low values so this test can complete in a sane amout of time
	cfg.MinimumImageDeletionAge = 15 * time.Minute

	taskEngine, done, _ := setup(cfg, nil, t)
	defer done()

	dockerClient := taskEngine.(*DockerTaskEngine).client

	// DockerClient doesn't implement TagImage, so create a go docker client
	sdkDockerClient, err := client.NewClientWithOpts(client.WithVersion(sdkclientfactory.GetDefaultVersion().String()))
	require.NoError(t, err, "Creating docker client failed")

	imageManager := taskEngine.(*DockerTaskEngine).imageManager.(*dockerImageManager)
	imageManager.SetDataClient(data.NewNoopClient())

	stateChangeEvents := taskEngine.StateChangeEvents()

	// Start three tasks which using the image with same ID and different Name
	task1 := createTestTask("task1")
	task2 := createTestTask("task2")
	task3 := createTestTask("task3")
	task1.Containers[0].Image = "testimagewithsameidanddifferentnames-1:latest"
	task2.Containers[0].Image = "testimagewithsameidanddifferentnames-2:latest"
	task3.Containers[0].Image = "testimagewithsameidanddifferentnames-3:latest"

	// Pull the images needed for the test
	if _, err = dockerClient.InspectImage(test4Image1Name); client.IsErrNotFound(err) {
		metadata := dockerClient.PullImage(ctx, test4Image1Name, nil, defaultTestConfigIntegTest().ImagePullTimeout)
		assert.NoError(t, metadata.Error, "Failed to pull image %s", test4Image1Name)
	}

	// Using testImage1Name for all the tasks but with different name
	err = renameImage(test4Image1Name, task1.Containers[0].Image, sdkDockerClient)
	require.NoError(t, err, "Renaming image failed")

	// Start and wait for task1 to be running
	go taskEngine.AddTask(task1)
	err = verifyTaskIsRunning(stateChangeEvents, task1)
	require.NoError(t, err)

	imageState1, ok := imageManager.GetImageStateFromImageName(task1.Containers[0].Image)
	require.True(t, ok, "Could not find image state for %s", task1.Containers[0].Image)
	t.Logf("Found image state for %s", task1.Containers[0].Image)
	imageID1 := imageState1.Image.ImageID

	// copy the image for task2 to run with same image but different name
	err = sdkDockerClient.ImageTag(ctx, task1.Containers[0].Image, task2.Containers[0].Image)
	require.NoError(t, err, "Trying to copy image failed")

	// Start and wait for task2 to be running
	go taskEngine.AddTask(task2)
	err = verifyTaskIsRunning(stateChangeEvents, task2)
	require.NoError(t, err)

	imageState2, ok := imageManager.GetImageStateFromImageName(task2.Containers[0].Image)
	require.True(t, ok, "Could not find image state for %s", task2.Containers[0].Image)
	t.Logf("Found image state for %s", task2.Containers[0].Image)
	imageID2 := imageState2.Image.ImageID
	require.Equal(t, imageID2, imageID1, "The image id in task2 should be same as in task1")

	// make task3 use the same image name but different image id
	err = sdkDockerClient.ImageTag(ctx, task1.Containers[0].Image, task3.Containers[0].Image)
	require.NoError(t, err, "Trying to copy image failed")

	// Start and wait for task3 to be running
	go taskEngine.AddTask(task3)
	err = verifyTaskIsRunning(stateChangeEvents, task3)
	assert.NoError(t, err)

	imageState3, ok := imageManager.GetImageStateFromImageName(task3.Containers[0].Image)
	require.True(t, ok, "Could not find image state for %s", task3.Containers[0].Image)
	t.Logf("Found image state for %s", task3.Containers[0].Image)
	imageID3 := imageState3.Image.ImageID
	require.Equal(t, imageID3, imageID1, "The image id in task3 should be the same as in task1")

	// Modify the image state so that the image is eligible for deletion
	// all the three tasks has the same imagestate
	imageState1.LastUsedAt = imageState1.LastUsedAt.Add(-99995 * time.Hour)
	imageState1.PulledAt = imageState1.PulledAt.Add(-20 * time.Minute)

	go discardEvents(stateChangeEvents)
	// Wait for the Task to be stopped
	waitForTaskStoppedByCheckStatus(task1)
	waitForTaskStoppedByCheckStatus(task2)
	waitForTaskStoppedByCheckStatus(task3)

	task1.SetSentStatus(apitaskstatus.TaskStopped)
	task2.SetSentStatus(apitaskstatus.TaskStopped)
	task3.SetSentStatus(apitaskstatus.TaskStopped)

	// Allow Task cleanup to occur
	time.Sleep(5 * time.Second)

	err = verifyTaskIsCleanedUp("task1", taskEngine)
	assert.NoError(t, err, "task1")
	err = verifyTaskIsCleanedUp("task2", taskEngine)
	assert.NoError(t, err, "task2")
	err = verifyTaskIsCleanedUp("task3", taskEngine)
	assert.NoError(t, err, "task3")

	imageManager.removeUnusedImages(ctx)

	// Verify all the images are removed from image manager
	err = verifyImagesAreRemoved(imageManager, imageID1)
	assert.NoError(t, err, "imageID1")

	// Verify images are removed by docker
	_, err = taskEngine.(*DockerTaskEngine).client.InspectImage(imageID1)
	assert.True(t, client.IsErrNotFound(err), "Image was not removed successfully")
}

// renameImage retag the image with the target tag and delete the source tag
func renameImage(source string, target string, client *client.Client) error {
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	err := client.ImageTag(ctx, source, target)
	if err != nil {
		return fmt.Errorf("Trying to tag image failed, err: %v", err)
	}

	// delete the source tag
	_, err = client.ImageRemove(ctx, source, types.ImageRemoveOptions{})
	if err != nil {
		return fmt.Errorf("Failed to remove the source tag of the image: %s", source)
	}

	return nil
}

func createImageCleanupHappyTestTask(taskName string) *apitask.Task {
	return &apitask.Task{
		Arn:                 taskName,
		Family:              taskName,
		Version:             "1",
		DesiredStatusUnsafe: apitaskstatus.TaskRunning,
		Containers: []*apicontainer.Container{
			{
				Name:                "test1",
				Image:               test1Image1Name,
				Essential:           false,
				DesiredStatusUnsafe: apicontainerstatus.ContainerRunning,
				CPU:                 512,
				Memory:              256,
			},
			{
				Name:                "test2",
				Image:               test1Image2Name,
				Essential:           false,
				DesiredStatusUnsafe: apicontainerstatus.ContainerRunning,
				CPU:                 512,
				Memory:              256,
			},
			{
				Name:                "test3",
				Image:               test1Image3Name,
				Essential:           false,
				DesiredStatusUnsafe: apicontainerstatus.ContainerRunning,
				CPU:                 512,
				Memory:              256,
			},
		},
	}
}

func createImageCleanupThresholdTestTask(taskName string) *apitask.Task {
	return &apitask.Task{
		Arn:                 taskName,
		Family:              taskName,
		Version:             "1",
		DesiredStatusUnsafe: apitaskstatus.TaskRunning,
		Containers: []*apicontainer.Container{
			{
				Name:                "test1",
				Image:               test2Image1Name,
				Essential:           false,
				DesiredStatusUnsafe: apicontainerstatus.ContainerRunning,
				CPU:                 512,
				Memory:              256,
			},
			{
				Name:                "test2",
				Image:               test2Image2Name,
				Essential:           false,
				DesiredStatusUnsafe: apicontainerstatus.ContainerRunning,
				CPU:                 512,
				Memory:              256,
			},
			{
				Name:                "test3",
				Image:               test2Image3Name,
				Essential:           false,
				DesiredStatusUnsafe: apicontainerstatus.ContainerRunning,
				CPU:                 512,
				Memory:              256,
			},
		},
	}
}

func verifyTaskIsCleanedUp(taskName string, taskEngine TaskEngine) error {
	for i := 0; i < taskCleanupTimeoutSeconds; i++ {
		_, ok := taskEngine.(*DockerTaskEngine).State().TaskByArn(taskName)
		if !ok {
			break
		}
		time.Sleep(1 * time.Second)
		if i == (taskCleanupTimeoutSeconds - 1) {
			return errors.New("Expected Task to have been swept but was not")
		}
	}
	return nil
}

func verifyImagesAreRemoved(imageManager *dockerImageManager, imageIDs ...string) error {
	imagesNotRemovedList := []string{}
	for _, imageID := range imageIDs {
		imageState, ok := imageManager.getImageState(imageID)
		if ok {
			imagesNotRemovedList = append(imagesNotRemovedList, imageState.Image.String())
		}
	}
	if len(imagesNotRemovedList) > 0 {
		return fmt.Errorf("Image states still exist for: %s", imagesNotRemovedList)
	}
	return nil
}

func verifyImagesAreNotRemoved(imageManager *dockerImageManager, imageIDs ...string) error {
	imagesRemovedList := []string{}
	for _, imageID := range imageIDs {
		imageState, ok := imageManager.getImageState(imageID)
		if !ok {
			imagesRemovedList = append(imagesRemovedList, imageState.Image.String())
		}
	}
	if len(imagesRemovedList) > 0 {
		return fmt.Errorf("Could not find images: %s in ImageManager", imagesRemovedList)
	}
	return nil
}

func cleanupImagesHappy(imageManager *dockerImageManager) {
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	imageManager.client.RemoveContainer(ctx, "test1", dockerclient.RemoveContainerTimeout)
	imageManager.client.RemoveContainer(ctx, "test2", dockerclient.RemoveContainerTimeout)
	imageManager.client.RemoveContainer(ctx, "test3", dockerclient.RemoveContainerTimeout)
	imageManager.client.RemoveImage(ctx, test1Image1Name, imageRemovalTimeout)
	imageManager.client.RemoveImage(ctx, test1Image2Name, imageRemovalTimeout)
	imageManager.client.RemoveImage(ctx, test1Image3Name, imageRemovalTimeout)
}

func cleanupImagesThreshold(imageManager *dockerImageManager) {
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	imageManager.client.RemoveContainer(ctx, "test1", dockerclient.RemoveContainerTimeout)
	imageManager.client.RemoveContainer(ctx, "test2", dockerclient.RemoveContainerTimeout)
	imageManager.client.RemoveContainer(ctx, "test3", dockerclient.RemoveContainerTimeout)
	imageManager.client.RemoveImage(ctx, test2Image1Name, imageRemovalTimeout)
	imageManager.client.RemoveImage(ctx, test2Image2Name, imageRemovalTimeout)
	imageManager.client.RemoveImage(ctx, test2Image3Name, imageRemovalTimeout)
}
