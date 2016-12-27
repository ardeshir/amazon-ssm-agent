// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package configurepackage implements the ConfigurePackage plugin.
package configurepackage

import (
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/fileutil/artifact"
	"github.com/aws/amazon-ssm-agent/agent/framework/runpluginutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/statemanager/model"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/amazon-ssm-agent/agent/updateutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

var loggerMock = log.NewMockLog()
var contextMock context.T = context.NewMockDefault()

func fileSysStubSuccess() fileSysDep {
	result, _ := ioutil.ReadFile("testdata/sampleManifest.json")
	return &FileSysDepStub{readResult: result, existsResultDefault: true}
}

func networkStubSuccess() networkDep {
	return &NetworkDepStub{downloadResultDefault: artifact.DownloadOutput{LocalFilePath: "Stub"}}
}

func execStubSuccess() execDep {
	return &ExecDepStub{pluginInput: &model.PluginState{}, pluginOutput: &contracts.PluginResult{Status: contracts.ResultStatusSuccess}}
}

func setSuccessStubs() *ConfigurePackageStubs {
	stubs := &ConfigurePackageStubs{fileSysDepStub: fileSysStubSuccess(), networkDepStub: networkStubSuccess(), execDepStub: execStubSuccess()}
	stubs.Set()
	return stubs
}

func TestRunUpgrade(t *testing.T) {
	plugin := &Plugin{}
	instanceContext := createStubInstanceContext()
	pluginInformation := createStubPluginInputInstall()

	managerMock := ConfigPackageSuccessMock("/foo", "1.0.0", "0.5.6", &PackageManifest{}, contracts.ResultStatusSuccess, contracts.ResultStatusSuccess, contracts.ResultStatusSuccess)
	output := runConfigurePackage(plugin, contextMock, managerMock, instanceContext, pluginInformation)

	assert.Equal(t, output.ExitCode, 0)
	assert.Contains(t, output.Stdout, "Successfully installed")
	managerMock.AssertCalled(t, "setMark", "PVDriver", "1.0.0")
	managerMock.AssertCalled(t, "runUninstallPackagePre", "PVDriver", "0.5.6", mock.Anything, mock.Anything)
	managerMock.AssertCalled(t, "runUninstallPackagePost", "PVDriver", "0.5.6", mock.Anything, mock.Anything)
	managerMock.AssertCalled(t, "clearMark", "PVDriver")
}

func TestRunUpgradeUninstallReboot(t *testing.T) {
	plugin := &Plugin{}
	instanceContext := createStubInstanceContext()
	pluginInformation := createStubPluginInputInstall()

	managerMock := ConfigPackageSuccessMock("/foo", "1.0.0", "0.5.6", &PackageManifest{}, contracts.ResultStatusSuccess, contracts.ResultStatusSuccessAndReboot, contracts.ResultStatusSuccess)
	output := runConfigurePackage(plugin, contextMock, managerMock, instanceContext, pluginInformation)

	assert.Equal(t, output.ExitCode, 0)
	managerMock.AssertCalled(t, "setMark", "PVDriver", "1.0.0")
	managerMock.AssertCalled(t, "runUninstallPackagePre", "PVDriver", "0.5.6", mock.Anything, mock.Anything)
	managerMock.AssertNotCalled(t, "runInstallPackage")
	managerMock.AssertNotCalled(t, "runUninstallPackagePost")
	managerMock.AssertNotCalled(t, "clearMark")
}

func TestRunParallelSamePackage(t *testing.T) {
	plugin := &Plugin{}
	instanceContext := createStubInstanceContext()
	pluginInformation := createStubPluginInputInstall()

	managerMockFirst := ConfigPackageSuccessMock("/foo", "Wait1.0.0", "", &PackageManifest{}, contracts.ResultStatusSuccess, contracts.ResultStatusSuccess, contracts.ResultStatusSuccess)
	managerMockSecond := ConfigPackageSuccessMock("/foo", "1.0.0", "", &PackageManifest{}, contracts.ResultStatusSuccess, contracts.ResultStatusSuccess, contracts.ResultStatusSuccess)

	var outputFirst ConfigurePackagePluginOutput
	var outputSecond ConfigurePackagePluginOutput
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		outputFirst = runConfigurePackage(plugin, contextMock, managerMockFirst, instanceContext, pluginInformation)
	}()
	// wait until first call is at getVersionToInstall
	_ = <-managerMockFirst.waitChan
	// start second call
	outputSecond = runConfigurePackage(plugin, contextMock, managerMockSecond, instanceContext, pluginInformation)
	// after second call completes, allow first call to continue
	managerMockFirst.waitChan <- true
	// wait until first call is complete
	wg.Wait()

	assert.Equal(t, outputFirst.ExitCode, 0)
	assert.Equal(t, outputSecond.ExitCode, 1)
	assert.True(t, strings.Contains(outputSecond.Stderr, `Package "PVDriver" is already in the process of action "Install"`))
}

func TestMarkAsSucceeded(t *testing.T) {
	output := ConfigurePackagePluginOutput{}

	output.MarkAsSucceeded(false)

	assert.Equal(t, output.ExitCode, 0)
	assert.Equal(t, output.Status, contracts.ResultStatusSuccess)
}

func TestMarkAsFailed(t *testing.T) {
	output := ConfigurePackagePluginOutput{}

	output.MarkAsFailed(loggerMock, fmt.Errorf("Error message"))

	assert.Equal(t, output.ExitCode, 1)
	assert.Equal(t, output.Status, contracts.ResultStatusFailed)
	assert.Contains(t, output.Stderr, "Error message")
}

func TestAppendInfo(t *testing.T) {
	output := ConfigurePackagePluginOutput{}

	output.AppendInfo(loggerMock, "Info message")
	output.AppendInfo(loggerMock, "Second entry")

	assert.Contains(t, output.Stdout, "Info message")
	assert.Contains(t, output.Stdout, "Second entry")
}

func TestExecute(t *testing.T) {
	pluginInformation := createStubPluginInputInstall()
	config := contracts.Configuration{}
	p := make([]interface{}, 1)
	p[0] = pluginInformation
	config.Properties = p
	plugin := &Plugin{}

	mockCancelFlag := new(task.MockCancelFlag)

	getContextOrig := getContext
	runConfigOrig := runConfig
	getContext = func(log log.T) (context *updateutil.InstanceContext, err error) {
		return createStubInstanceContext(), nil
	}
	runConfig = func(
		p *Plugin,
		context context.T,
		manager configurePackageManager,
		instanceContext *updateutil.InstanceContext,
		rawPluginInput interface{}) (out ConfigurePackagePluginOutput) {
		out = ConfigurePackagePluginOutput{}
		out.ExitCode = 1
		out.Stderr = "error"

		return out
	}
	defer func() {
		runConfig = runConfigOrig
		getContext = getContextOrig
	}()

	// TODO:MF Test result code for reboot in cases where that is expected?

	// Setup mocks
	mockCancelFlag.On("Canceled").Return(false)
	mockCancelFlag.On("ShutDown").Return(false)
	mockCancelFlag.On("Wait").Return(false).After(100 * time.Millisecond)

	result := plugin.Execute(contextMock, config, mockCancelFlag, runpluginutil.PluginRunner{})

	assert.Equal(t, result.Code, 1)
	assert.Contains(t, result.Output, "error")
}

func TestInstallPackage(t *testing.T) {
	pluginInformation := createStubPluginInputInstall()

	output := &ConfigurePackagePluginOutput{}
	manager := createInstance()

	result, _ := ioutil.ReadFile("testdata/sampleManifest.json")
	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{readResult: result}, networkDepStub: &NetworkDepStub{}, execDepStub: execStubSuccess()}
	stubs.Set()
	defer stubs.Clear()

	_, err := manager.runInstallPackage(contextMock,
		pluginInformation.Name,
		pluginInformation.Version,
		output)

	assert.NoError(t, err)
}

func TestUninstallPackage(t *testing.T) {
	manager := createInstance()
	pluginInformation := createStubPluginInputUninstall()

	output := &ConfigurePackagePluginOutput{}

	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{existsResultDefault: true}, networkDepStub: &NetworkDepStub{}, execDepStub: execStubSuccess()}
	stubs.Set()
	defer stubs.Clear()

	_, errPre := manager.runUninstallPackagePre(contextMock,
		pluginInformation.Name,
		pluginInformation.Version,
		output)

	assert.NoError(t, errPre)

	_, errPost := manager.runUninstallPackagePost(contextMock,
		pluginInformation.Name,
		pluginInformation.Version,
		output)

	assert.NoError(t, errPost)
}

// TO DO: Uninstall test for exe command

func TestValidateInput(t *testing.T) {
	input := ConfigurePackagePluginInput{}

	input.Version = "1.0.0"
	input.Name = "PVDriver"
	input.Action = "InvalidAction"

	manager := createInstance()

	result, err := manager.validateInput(contextMock, &input)

	assert.True(t, result)
	assert.NoError(t, err)
}

func TestValidateInput_Source(t *testing.T) {
	input := ConfigurePackagePluginInput{}

	input.Version = "1.0.0"
	input.Name = "PVDriver"
	input.Action = "Install"
	input.Source = "http://amazon.com"

	manager := createInstance()

	result, err := manager.validateInput(contextMock, &input)

	assert.False(t, result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "source parameter is not supported")
}

func TestValidateInput_NameEmpty(t *testing.T) {
	input := ConfigurePackagePluginInput{}

	// Set version to a large number to avoid conflict of the actual package release version
	input.Version = "9000.0.0"
	input.Name = ""
	input.Action = "Install"

	manager := createInstance()
	result, err := manager.validateInput(contextMock, &input)

	assert.False(t, result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty name field")
}

func TestValidateInput_NameInvalid(t *testing.T) {
	input := ConfigurePackagePluginInput{}

	// Set version to a large number to avoid conflict of the actual package release version
	input.Version = "9000.0.0"
	input.Name = "."
	input.Action = "Install"

	invalidNames := []string{".", ".abc", "-", "-abc", "abc.", "abc-", "0abc", "1234", "../foo", "abc..def"}

	for _, name := range invalidNames {
		input.Name = name

		manager := createInstance()
		result, err := manager.validateInput(contextMock, &input)

		assert.False(t, result)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid name")
	}
}

func TestValidateInput_NameValid(t *testing.T) {
	input := ConfigurePackagePluginInput{}
	// Set version to a large number to avoid conflict of the actual package release version
	input.Version = "9000.0.0"
	input.Action = "Install"

	validNames := []string{"a0", "_a", "_._._", "_-_", "A", "ABCDEFGHIJKLM-NOPQRSTUVWXYZ.abcdefghijklm-nopqrstuvwxyz.1234567890"}

	for _, name := range validNames {
		input.Name = name

		manager := createInstance()
		result, err := manager.validateInput(contextMock, &input)

		assert.True(t, result)
		assert.NoError(t, err)
	}
}

func TestValidateInput_Version(t *testing.T) {
	input := ConfigurePackagePluginInput{}

	// Set version to a large number to avoid conflict of the actual package release version
	input.Version = "9000.0.0.0"
	input.Name = "PVDriver"
	input.Action = "Install"

	manager := createInstance()
	result, err := manager.validateInput(contextMock, &input)

	assert.False(t, result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid version")
}

func TestValidateInput_EmptyVersionWithInstall(t *testing.T) {
	input := ConfigurePackagePluginInput{}

	// Set version to a large number to avoid conflict of the actual package release version
	input.Version = ""
	input.Name = "PVDriver"
	input.Action = "Install"

	manager := createInstance()
	result, err := manager.validateInput(contextMock, &input)

	assert.True(t, result)
	assert.NoError(t, err)
}

func TestValidateInput_EmptyVersionWithUninstall(t *testing.T) {
	input := ConfigurePackagePluginInput{}

	// Set version to a large number to avoid conflict of the actual package release version
	input.Version = ""
	input.Name = "PVDriver"
	input.Action = "Uninstall"

	manager := createInstance()
	result, err := manager.validateInput(contextMock, &input)

	assert.True(t, result)
	assert.NoError(t, err)
}

func TestDownloadPackage(t *testing.T) {
	pluginInformation := createStubPluginInputInstall()

	output := ConfigurePackagePluginOutput{}
	manager := createInstance()
	util := mockConfigureUtility{}

	result := artifact.DownloadOutput{}
	result.LocalFilePath = "packages/PVDriver/9000.0.0/PVDriver.zip"

	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{}, networkDepStub: &NetworkDepStub{downloadResultDefault: result}}
	stubs.Set()
	defer stubs.Clear()

	fileName, err := manager.downloadPackage(contextMock, &util, pluginInformation.Name, pluginInformation.Version, &output)

	assert.Equal(t, "packages/PVDriver/9000.0.0/PVDriver.zip", fileName)
	assert.NoError(t, err)
}

func TestDownloadPackage_Failed(t *testing.T) {
	pluginInformation := createStubPluginInputInstall()

	output := ConfigurePackagePluginOutput{}
	manager := createInstance()
	util := mockConfigureUtility{}

	// file download failed
	result := artifact.DownloadOutput{}
	result.LocalFilePath = ""

	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{}, networkDepStub: &NetworkDepStub{downloadResultDefault: result, downloadErrorDefault: errors.New("404")}}
	stubs.Set()
	defer stubs.Clear()

	fileName, err := manager.downloadPackage(contextMock, &util, pluginInformation.Name, pluginInformation.Version, &output)

	assert.Empty(t, fileName)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to download installation package reliably")
	assert.Contains(t, err.Error(), "404")
}

func TestPackageLock(t *testing.T) {
	// lock Foo for Install
	err := lockPackage("Foo", "Install")
	assert.Nil(t, err)
	defer unlockPackage("Foo")

	// shouldn't be able to lock Foo, even for a different action
	err = lockPackage("Foo", "Uninstall")
	assert.NotNil(t, err)

	// lock and unlock Bar (with defer)
	err = lockAndUnlock("Bar")
	assert.Nil(t, err)

	// should be able to lock and then unlock Bar
	err = lockPackage("Bar", "Uninstall")
	assert.Nil(t, err)
	unlockPackage("Bar")

	// should be able to lock Bar
	err = lockPackage("Bar", "Uninstall")
	assert.Nil(t, err)
	defer unlockPackage("Bar")

	// lock in a goroutine with a 10ms sleep
	errorChan := make(chan error)
	go lockAndUnlockGo("Foobar", errorChan)
	err = <-errorChan // wait until the goroutine has acquired the lock
	assert.Nil(t, err)
	err = lockPackage("Foobar", "Install")
	errorChan <- err // signal the goroutine to exit
	assert.NotNil(t, err)
}

func TestPackageMark(t *testing.T) {
	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{existsResultDefault: false}}
	stubs.Set()
	defer stubs.Clear()

	err := markInstallingPackage("Foo", "999.999.999")
	assert.Nil(t, err)
}

func TestPackageInstalling(t *testing.T) {
	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{existsResultDefault: true, readResult: []byte("999.999.999")}}
	stubs.Set()
	defer stubs.Clear()

	assert.Equal(t, "999.999.999", getInstallingPackageVersion("Foo"))
}

func TestPackageNotInstalling(t *testing.T) {
	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{existsResultDefault: false}}
	stubs.Set()
	defer stubs.Clear()

	assert.Equal(t, "", getInstallingPackageVersion("Foo"))
}

func TestPackageUnreadableInstalling(t *testing.T) {
	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{existsResultDefault: false, readResult: []byte(""), readError: errors.New("Foo")}}
	stubs.Set()
	defer stubs.Clear()

	assert.Equal(t, "", getInstallingPackageVersion("Foo"))
}

func TestUnmarkPackage(t *testing.T) {
	stubs := &ConfigurePackageStubs{fileSysDepStub: &FileSysDepStub{existsResultDefault: true}}
	stubs.Set()
	defer stubs.Clear()

	assert.Nil(t, unmarkInstallingPackage("Foo"))
}

func lockAndUnlockGo(packageName string, channel chan error) {
	err := lockPackage(packageName, "Install")
	channel <- err
	_ = <-channel
	if err == nil {
		defer unlockPackage(packageName)
	}
	return
}

func lockAndUnlock(packageName string) (err error) {
	if err = lockPackage(packageName, "Install"); err != nil {
		return
	}
	defer unlockPackage(packageName)
	return
}
