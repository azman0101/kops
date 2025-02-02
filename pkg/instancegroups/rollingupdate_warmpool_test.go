/*
Copyright 2021 The Kubernetes Authors.

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

package instancegroups

import (
	"context"
	"testing"

	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kops/cloudmock/aws/mockautoscaling"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/cloudinstances"
	"k8s.io/kops/pkg/validation"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/util/pkg/awsinterfaces"
)

// Here we have three nodes that are up to date, while three warm nodes need updating.
// Only the initial cluster validation should be run
func TestRollingUpdateOnlyWarmPoolNodes(t *testing.T) {
	ctx := context.TODO()
	c, cloud := getTestSetup()
	k8sClient := c.K8sClient
	groups := make(map[string]*cloudinstances.CloudInstanceGroup)
	makeGroupWithWarmPool(groups, k8sClient, cloud, "node-1", kops.InstanceGroupRoleNode, 3, 0, 3, 3)

	validator := &countingValidator{}
	c.ClusterValidator = validator

	assert.Equal(t, 3, len(groups["node-1"].NeedUpdate), "number of nodes needing update")

	err := c.RollingUpdate(ctx, groups, &kops.InstanceGroupList{})
	assert.NoError(t, err, "rolling update")
	assert.Equal(t, 1, validator.numValidations, "number of validations")
}

func TestRollingWarmPoolBeforeJoinedNodes(t *testing.T) {
	ctx := context.TODO()
	c, cloud := getTestSetup()
	k8sClient := c.K8sClient
	groups := make(map[string]*cloudinstances.CloudInstanceGroup)
	makeGroupWithWarmPool(groups, k8sClient, cloud, "node-1", kops.InstanceGroupRoleNode, 3, 3, 3, 3)

	warmPoolBeforeJoinedNodesTest := &warmPoolBeforeJoinedNodesTest{
		EC2API: cloud.MockEC2,
		t:      t,
	}
	cloud.MockEC2 = warmPoolBeforeJoinedNodesTest

	err := c.RollingUpdate(ctx, groups, &kops.InstanceGroupList{})

	assert.NoError(t, err, "rolling update")

	assert.Equal(t, 6, warmPoolBeforeJoinedNodesTest.numTerminations, "Number of terminations")
}

func TestRollingUpdateWarmPoolAMIUpdate(t *testing.T) {
	ctx := context.TODO()
	c, cloud := getTestSetup()
	k8sClient := c.K8sClient
	groups := make(map[string]*cloudinstances.CloudInstanceGroup)

	// 1. Create instance group with initial AMI and warm pool configuration:
	// - 3 active instances (0 needing update)
	// - 3 warm pool instances (all needing update)
	makeGroupWithWarmPool(groups, k8sClient, cloud, "node-ami", kops.InstanceGroupRoleNode, 3, 0, 3, 3)
	group := groups["node-ami"]
	group.InstanceGroup.Spec.Image = "ssm:/initial/ami-id"  // Set initial AMI


    // Mock SSM parameter resolution
    mockSSM := cloud.MockSSM.(*mockssm.MockSSM)
    mockSSM.Parameters["/initial/ami-id"] = &ssmtypes.Parameter{
        Value: aws.String("ami-initial"),
    }
	// 2. Verify initial state - only warm pool instances need updates
	assert.Equal(t, 3, len(group.NeedUpdate), "initial outdated instances should only be warm pool nodes")

    // 3. Update SSM parameter value without changing the spec
    mockSSM.Parameters["/initial/ami-id"].Value = aws.String("ami-updated")

	// 4. Verify both active and warm pool instances now require updates:
	// - 3 previously up-to-date active instances now need update
	// - 3 existing warm pool instances still need update
	assert.Equal(t, 6, len(group.NeedUpdate), "all instances (active + warm pool) should need update after AMI change")

	// 5. Execute rolling update process to replace outdated instances
	err := c.RollingUpdate(ctx, groups, &kops.InstanceGroupList{})
	assert.NoError(t, err)

	// 6. Verify AWS Auto Scaling Group launch template was updated with new AMI
	// This ensures new instances will be launched with the updated image
	mockASG := cloud.MockAutoscaling.(*mockautoscaling.MockAutoscaling)
	asg := mockASG.Groups["node-ami"]
	assert.Equal(t, "ami-updated", asg.LaunchTemplate.ImageId, "ASG launch template should reflect new AMI")
}

type countingValidator struct {
	numValidations int
}

func (c *countingValidator) Validate(ctx context.Context) (*validation.ValidationCluster, error) {
	c.numValidations++
	return &validation.ValidationCluster{}, nil
}

func makeGroupWithWarmPool(groups map[string]*cloudinstances.CloudInstanceGroup, k8sClient kubernetes.Interface, cloud *awsup.MockAWSCloud, name string, role kops.InstanceGroupRole, count int, needUpdate int, warmCount int, warmNeedUpdate int) {
	makeGroup(groups, k8sClient, cloud, name, role, count, needUpdate)

	group := groups[name]

	wpInstances := []autoscalingtypes.Instance{}
	for i := 0; i < warmCount; i++ {
		id := name + "-wp-" + string(rune('a'+i))
		instance := autoscalingtypes.Instance{
			InstanceId:     &id,
			LifecycleState: autoscalingtypes.LifecycleStateWarmedStopped,
		}
		wpInstances = append(wpInstances, instance)

		cm, _ := group.NewCloudInstance(id, cloudinstances.CloudInstanceStatusNeedsUpdate, nil)
		cm.State = cloudinstances.WarmPool

	}

	// There is no API to write to warm pools, so we need to cheat.
	mockASG := cloud.MockAutoscaling.(*mockautoscaling.MockAutoscaling)
	mockASG.WarmPoolInstances[name] = wpInstances
}

type warmPoolBeforeJoinedNodesTest struct {
	awsinterfaces.EC2API
	t               *testing.T
	numTerminations int
}

func (t *warmPoolBeforeJoinedNodesTest) TerminateInstances(ctx context.Context, input *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	t.numTerminations++

	return t.EC2API.TerminateInstances(ctx, input, optFns...)
}
