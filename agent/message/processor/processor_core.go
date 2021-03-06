// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package processor implements MDS plugin processor
// processor_core contains functions that fetch messages and schedule them to be executed
package processor

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/framework/runpluginutil"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	messageContracts "github.com/aws/amazon-ssm-agent/agent/message/contracts"
	"github.com/aws/amazon-ssm-agent/agent/message/parser"
	"github.com/aws/amazon-ssm-agent/agent/message/service"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/sdkutil"
	commandStateHelper "github.com/aws/amazon-ssm-agent/agent/statemanager"
	"github.com/aws/amazon-ssm-agent/agent/statemanager/model"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/aws-sdk-go/service/ssmmds"
)

var singletonMapOfUnsupportedSSMDocs map[string]bool
var once sync.Once

var loadDocStateFromSendCommand = parseSendCommandMessage
var loadDocStateFromCancelCommand = parseCancelCommandMessage

// runCmdsUsingCmdState takes commandState as an input and executes only those plugins which haven't yet executed. This is functionally
// very similar to processSendCommandMessage because everything to do with cmd execution is part of that function right now.
func (p *Processor) runCmdsUsingCmdState(context context.T,
	mdsService service.Service,
	runPlugins PluginRunner,
	cancelFlag task.CancelFlag,
	buildReply replyBuilder,
	sendResponse runpluginutil.SendResponse,
	docState model.DocumentState) {

	log := context.Log()

	//Since only some plugins of a cmd gets executed here - there is no need to get output from engine & construct the sendReply output.
	//Instead after all plugins of a command get executed, use persisted data to construct sendReply payload
	outputs := runPlugins(context, docState.DocumentInformation.MessageID, docState.InstancePluginsInformation, sendResponse, cancelFlag)

	payloadDoc := buildReply("", outputs)

	//read from persisted file
	newCmdState := commandStateHelper.GetDocumentInterimState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent)

	// set document level information which wasn't set previously
	newCmdState.DocumentInformation.AdditionalInfo = payloadDoc.AdditionalInfo
	newCmdState.DocumentInformation.DocumentStatus = payloadDoc.DocumentStatus
	newCmdState.DocumentInformation.DocumentTraceOutput = payloadDoc.DocumentTraceOutput
	newCmdState.DocumentInformation.RuntimeStatus = payloadDoc.RuntimeStatus

	//persist final documentInfo.
	commandStateHelper.PersistDocumentInfo(log,
		newCmdState.DocumentInformation,
		newCmdState.DocumentInformation.DocumentID,
		newCmdState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent)

	pluginOutputContent, _ := jsonutil.Marshal(outputs)
	log.Debugf("plugin outputs %v", jsonutil.Indent(pluginOutputContent))

	//send document level reply
	log.Debug("sending reply on message completion ", outputs)
	sendResponse(newCmdState.DocumentInformation.MessageID, "", outputs)

	// Skip sending response when the document requires a reboot
	if newCmdState.DocumentInformation.DocumentStatus == contracts.ResultStatusSuccessAndReboot {
		log.Debugf("skipping moving interimState file %v since the document requires a reboot", newCmdState.DocumentInformation.CommandID)
		return
	}

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("execution of %v is over. Moving interimState file from Current to Completed folder", newCmdState.DocumentInformation.MessageID)

	commandStateHelper.MoveDocumentState(log,
		newCmdState.DocumentInformation.DocumentID,
		newCmdState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("deleting message")

	if !isUpdatePlugin(newCmdState) {
		err := mdsService.DeleteMessage(log, newCmdState.DocumentInformation.MessageID)
		if err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
	} else {
		log.Debug("messageDeletion skipped as it will be handled by external process")
	}
}

func (p *Processor) processMessage(msg *ssmmds.Message) {
	var (
		docState *model.DocumentState
		err      error
	)

	// create separate logger that includes messageID with every log message
	context := p.context.With("[messageID=" + *msg.MessageId + "]")
	log := context.Log()
	log.Debug("Processing message")

	if err = validate(msg); err != nil {
		log.Error("message not valid, ignoring: ", err)
		return
	}

	if strings.HasPrefix(*msg.Topic, string(SendCommandTopicPrefix)) {
		docState, err = loadDocStateFromSendCommand(context, msg, p.orchestrationRootDir)
		if err != nil {
			log.Error(err)
			p.sendDocLevelResponse(*msg.MessageId, contracts.ResultStatusFailed, err.Error())
			return
		}
	} else if strings.HasPrefix(*msg.Topic, string(CancelCommandTopicPrefix)) {
		docState, err = loadDocStateFromCancelCommand(context, msg, p.orchestrationRootDir)
	} else {
		err = fmt.Errorf("unexpected topic name %v", *msg.Topic)
	}

	if err != nil {
		log.Error("format of received message is invalid ", err)
		if err = p.service.FailMessage(log, *msg.MessageId, service.InternalHandlerException); err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
		return
	}

	//persisting received msg in file-system [pending folder]
	p.persistData(docState, appconfig.DefaultLocationOfPending)
	if err = p.service.AcknowledgeMessage(log, *msg.MessageId); err != nil {
		sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		return
	}

	log.Debugf("Ack done. Received message - messageId - %v, MessageString - %v", *msg.MessageId, msg.GoString())
	log.Debugf("Processing to send a reply to update the document status to InProgress")

	p.sendDocLevelResponse(*msg.MessageId, contracts.ResultStatusInProgress, "")

	log.Debugf("SendReply done. Received message - messageId - %v, MessageString - %v", *msg.MessageId, msg.GoString())

	p.ExecutePendingDocument(docState)
}

// submitDocForExecution moves doc to current folder and submit it for execution
func (p *Processor) ExecutePendingDocument(docState *model.DocumentState) {
	log := p.context.Log()

	commandStateHelper.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfPending,
		appconfig.DefaultLocationOfCurrent)

	switch docState.DocumentType {
	case model.SendCommand, model.SendCommandOffline:
		err := p.sendCommandPool.Submit(log, docState.DocumentInformation.MessageID, func(cancelFlag task.CancelFlag) {
			p.processSendCommandMessage(
				p.context,
				p.service,
				p.orchestrationRootDir,
				p.pluginRunner,
				cancelFlag,
				p.buildReply,
				p.sendResponse,
				docState)
		})
		if err != nil {
			log.Error("SendCommand failed", err)
			return
		}

	case model.CancelCommand, model.CancelCommandOffline:
		err := p.cancelCommandPool.Submit(log, docState.DocumentInformation.MessageID, func(cancelFlag task.CancelFlag) {
			p.processCancelCommandMessage(p.context, p.service, p.sendCommandPool, docState)
		})
		if err != nil {
			log.Error("CancelCommand failed", err)
			return
		}

	default:
		log.Error("unexpected document type ", docState.DocumentType)
	}
}

// loadPluginConfigurations returns plugin configurations that hasn't been executed
func loadPluginConfigurations(log log.T, plugins map[string]model.PluginState, commandID string) map[string]*contracts.Configuration {
	configs := make(map[string]*contracts.Configuration)

	for pluginName, pluginConfig := range plugins {
		if pluginConfig.HasExecuted {
			log.Debugf("skipping execution of Plugin - %v of command - %v since it has already executed.", pluginName, commandID)
			continue
		}
		log.Debugf("Plugin - %v of command - %v will be executed", pluginName, commandID)
		config := pluginConfig.Configuration
		configs[pluginName] = &config
	}

	return configs
}

// processSendCommandMessage processes a single send command message received from MDS.
func (p *Processor) processSendCommandMessage(context context.T,
	mdsService service.Service,
	messagesOrchestrationRootDir string,
	runPlugins PluginRunner,
	cancelFlag task.CancelFlag,
	buildReply replyBuilder,
	sendResponse runpluginutil.SendResponse,
	docState *model.DocumentState) {

	log := context.Log()

	log.Debug("Running plugins...")
	outputs := runPlugins(context, docState.DocumentInformation.MessageID, docState.InstancePluginsInformation, sendResponse, cancelFlag)
	pluginOutputContent, _ := jsonutil.Marshal(outputs)
	log.Debugf("Plugin outputs %v", jsonutil.Indent(pluginOutputContent))

	payloadDoc := buildReply("", outputs)

	//update documentInfo in interim cmd state file
	newCmdState := commandStateHelper.GetDocumentInterimState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent)

	// set document level information which wasn't set previously
	newCmdState.DocumentInformation.AdditionalInfo = payloadDoc.AdditionalInfo
	newCmdState.DocumentInformation.DocumentStatus = payloadDoc.DocumentStatus
	newCmdState.DocumentInformation.DocumentTraceOutput = payloadDoc.DocumentTraceOutput
	newCmdState.DocumentInformation.RuntimeStatus = payloadDoc.RuntimeStatus

	//persist final documentInfo.
	commandStateHelper.PersistDocumentInfo(log,
		newCmdState.DocumentInformation,
		newCmdState.DocumentInformation.DocumentID,
		newCmdState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent)

	log.Debug("Sending reply on message completion ", outputs)
	sendResponse(newCmdState.DocumentInformation.MessageID, "", outputs)

	// Skip sending response when the document requires a reboot
	if newCmdState.DocumentInformation.DocumentStatus == contracts.ResultStatusSuccessAndReboot {
		log.Debugf("skipping moving interimState file %v since the document requires a reboot", newCmdState.DocumentInformation.CommandID)
		return
	}

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("execution of %v is over. Moving interimState file from Current to Completed folder", newCmdState.DocumentInformation.MessageID)

	commandStateHelper.MoveDocumentState(log,
		newCmdState.DocumentInformation.DocumentID,
		newCmdState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("Deleting message")

	if !isUpdatePlugin(newCmdState) {
		if err := mdsService.DeleteMessage(log, newCmdState.DocumentInformation.MessageID); err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
	} else {
		log.Debug("MessageDeletion skipped as it will be handled by external process")
	}
}

func parseSendCommandMessage(context context.T, msg *ssmmds.Message, messagesOrchestrationRootDir string) (*model.DocumentState, error) {
	log := context.Log()
	commandID := getCommandID(*msg.MessageId)

	log.Debug("Processing send command message ", *msg.MessageId)
	log.Trace("Processing send command message ", jsonutil.Indent(*msg.Payload))

	parsedMessage, err := parser.ParseMessageWithParams(log, *msg.Payload)
	if err != nil {
		return nil, err
	}

	parsedMessageContent, _ := jsonutil.Marshal(parsedMessage)
	log.Debug("ParsedMessage is ", jsonutil.Indent(parsedMessageContent))

	// adapt plugin configuration format from MDS to plugin expected format
	s3KeyPrefix := path.Join(parsedMessage.OutputS3KeyPrefix, parsedMessage.CommandID, *msg.Destination)

	messageOrchestrationDirectory := filepath.Join(messagesOrchestrationRootDir, commandID)

	//persist : all information in current folder
	log.Info("Persisting message in current execution folder")

	//Data format persisted in Current Folder is defined by the struct - CommandState
	docState := initializeSendCommandState(parsedMessage, messageOrchestrationDirectory, s3KeyPrefix, *msg)

	var docStateContent string
	if docStateContent, err = jsonutil.Marshal(docState); err != nil {
		return nil, err
	}
	log.Debug("Document state is ", jsonutil.Indent(docStateContent))

	// Check if it is a managed instance and its executing managed instance incompatible AWS SSM public document.
	// A few public AWS SSM documents contain code which is not compatible when run on managed instances.
	// isManagedInstanceIncompatibleAWSSSMDocument makes sure to find such documents at runtime and replace the incompatible code.
	isMI, err := platform.IsManagedInstance()
	if err != nil {
		log.Errorf("Error determining managed instance. error: %v", err)
	}

	if isMI && model.IsManagedInstanceIncompatibleAWSSSMDocument(docState.DocumentInformation.DocumentName) {
		log.Debugf("Running incompatible AWS SSM Document %v on managed instance", docState.DocumentInformation.DocumentName)
		if err = model.RemoveDependencyOnInstanceMetadata(context, &docState); err != nil {
			return nil, err
		}
	}

	return &docState, nil
}

// processCancelCommandMessage processes a single send command message received from MDS.
func (p *Processor) processCancelCommandMessage(context context.T,
	mdsService service.Service,
	sendCommandPool task.Pool,
	docState *model.DocumentState) {

	log := context.Log()

	log.Debugf("Canceling job with id %v...", docState.CancelInformation.CancelMessageID)

	if found := sendCommandPool.Cancel(docState.CancelInformation.CancelMessageID); !found {
		log.Debugf("Job with id %v not found (possibly completed)", docState.CancelInformation.CancelMessageID)
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v couldn't be cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusFailed
	} else {
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusSuccess
	}

	//persist the final status of cancel-message in current folder
	commandStateHelper.PersistData(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent, docState)

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("Execution of %v is over. Moving interimState file from Current to Completed folder", docState.DocumentInformation.MessageID)

	commandStateHelper.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("Deleting message")
	if err := mdsService.DeleteMessage(log, docState.DocumentInformation.MessageID); err != nil {
		sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
	}
}

func parseCancelCommandMessage(context context.T, msg *ssmmds.Message, messagesOrchestrationRootDir string) (*model.DocumentState, error) {
	log := context.Log()

	log.Debug("Processing cancel command message ", jsonutil.Indent(*msg.Payload))

	var parsedMessage messageContracts.CancelPayload
	err := json.Unmarshal([]byte(*msg.Payload), &parsedMessage)
	if err != nil {
		return nil, err
	}
	log.Debugf("ParsedMessage is %v", parsedMessage)

	//persist in current folder here
	docState := initializeCancelCommandState(*msg, parsedMessage)
	return &docState, nil
}

func isUpdatePlugin(pluginConfig model.DocumentState) bool {
	for _, pluginState := range pluginConfig.InstancePluginsInformation {
		if pluginState.Name == appconfig.PluginEC2ConfigUpdate || pluginState.Name == appconfig.PluginNameAwsAgentUpdate {
			return true
		}
	}
	return false
}
