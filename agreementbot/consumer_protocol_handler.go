package agreementbot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/abstractprotocol"
	"github.com/open-horizon/anax/agreementbot/persistence"
	"github.com/open-horizon/anax/agreementbot/secrets"
	"github.com/open-horizon/anax/basicprotocol"
	"github.com/open-horizon/anax/compcheck"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/cutil"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/externalpolicy"
	"github.com/open-horizon/anax/i18n"
	"github.com/open-horizon/anax/metering"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/worker"
	"net/http"
	"time"
)

func CreateConsumerPH(name string, cfg *config.HorizonConfig, db persistence.AgbotDatabase, pm *policy.PolicyManager, msgq chan events.Message, mmsObjMgr *MMSObjectPolicyManager, secretsMgr secrets.AgbotSecrets, nodeSearch *NodeSearch) ConsumerProtocolHandler {
	if handler := NewBasicProtocolHandler(name, cfg, db, pm, msgq, mmsObjMgr, secretsMgr, nodeSearch); handler != nil {
		return handler
	} // Add new consumer side protocol handlers here
	return nil
}

type ConsumerProtocolHandler interface {
	Initialize()
	Name() string
	AcceptCommand(cmd worker.Command) bool
	AgreementProtocolHandler(typeName string, name string, org string) abstractprotocol.ProtocolHandler
	WorkQueue() *PrioritizedWorkQueue
	DispatchProtocolMessage(cmd *NewProtocolMessageCommand, cph ConsumerProtocolHandler) error
	PersistAgreement(wi *InitiateAgreement, proposal abstractprotocol.Proposal, workerID string) error
	PersistReply(reply abstractprotocol.ProposalReply, pol *policy.Policy, workerID string) error
	HandleAgreementTimeout(cmd *AgreementTimeoutCommand, cph ConsumerProtocolHandler)
	HandleBlockchainEvent(cmd *BlockchainEventCommand)
	HandlePolicyChanged(cmd *PolicyChangedCommand, cph ConsumerProtocolHandler)
	HandlePolicyDeleted(cmd *PolicyDeletedCommand, cph ConsumerProtocolHandler)
	HandleServicePolicyChanged(cmd *ServicePolicyChangedCommand, cph ConsumerProtocolHandler)
	HandleServicePolicyDeleted(cmd *ServicePolicyDeletedCommand, cph ConsumerProtocolHandler)
	HandleNodePolicyChanged(cmd *NodePolicyChangedCommand, cph ConsumerProtocolHandler)
	HandleMMSObjectPolicy(cmd *MMSObjectPolicyEventCommand, cph ConsumerProtocolHandler)
	HandleWorkloadUpgrade(cmd *WorkloadUpgradeCommand, cph ConsumerProtocolHandler)
	HandleMakeAgreement(cmd *MakeAgreementCommand, cph ConsumerProtocolHandler)
	HandleStopProtocol(cph ConsumerProtocolHandler)
	GetTerminationCode(reason string) uint
	GetTerminationReason(code uint) string
	IsTerminationReasonNodeShutdown(code uint) bool
	GetSendMessage() func(mt interface{}, pay []byte) error
	RecordConsumerAgreementState(agreementId string, pol *policy.Policy, org string, state string, workerID string) error
	DeleteMessage(msgId int) error
	CreateMeteringNotification(mp policy.Meter, agreement *persistence.Agreement) (*metering.MeteringNotification, error)
	TerminateAgreement(agreement *persistence.Agreement, reason uint, workerId string)
	VerifyAgreement(ag *persistence.Agreement, cph ConsumerProtocolHandler)
	UpdateAgreement(ag *persistence.Agreement, updateType string, metadata interface{}, cph ConsumerProtocolHandler)
	GetDeviceMessageEndpoint(deviceId string, workerId string) (string, []byte, error)
	SetBlockchainClientAvailable(ev *events.BlockchainClientInitializedMessage)
	SetBlockchainClientNotAvailable(ev *events.BlockchainClientStoppingMessage)
	SetBlockchainWritable(ev *events.AccountFundedMessage)
	IsBlockchainWritable(typeName string, name string, org string) bool
	CanCancelNow(agreement *persistence.Agreement) bool
	DeferCommand(cmd AgreementWork)
	GetDeferredCommands() []AgreementWork
	HandleDeferredCommands()
	PostReply(agreementId string, proposal abstractprotocol.Proposal, reply abstractprotocol.ProposalReply, consumerPolicy *policy.Policy, org string, workerId string) error
	UpdateProducer(ag *persistence.Agreement)
	HandleExtensionMessage(cmd *NewProtocolMessageCommand) error
	AlreadyReceivedReply(ag *persistence.Agreement) bool
	GetKnownBlockchain(ag *persistence.Agreement) (string, string, string)
	CanSendMeterRecord(ag *persistence.Agreement) bool
	GetExchangeId() string
	GetExchangeToken() string
	GetExchangeURL() string
	GetCSSURL() string
	GetAgbotURL() string
	GetServiceBased() bool
	GetHTTPFactory() *config.HTTPClientFactory
	SendEventMessage(event events.Message)
}

type BaseConsumerProtocolHandler struct {
	name             string
	pm               *policy.PolicyManager
	db               persistence.AgbotDatabase
	config           *config.HorizonConfig
	httpClient       *http.Client // shared HTTP client instance
	agbotId          string
	token            string
	deferredCommands []AgreementWork // The agreement related work that has to be deferred and retried
	messages         chan events.Message
	mmsObjMgr        *MMSObjectPolicyManager
	secretsMgr       secrets.AgbotSecrets
	nodeSearch       *NodeSearch
}

func (b *BaseConsumerProtocolHandler) GetSendMessage() func(mt interface{}, pay []byte) error {
	return b.sendMessage
}

func (b *BaseConsumerProtocolHandler) Name() string {
	return b.name
}

func (b *BaseConsumerProtocolHandler) GetExchangeId() string {
	return b.agbotId
}

func (b *BaseConsumerProtocolHandler) GetExchangeToken() string {
	return b.token
}

func (b *BaseConsumerProtocolHandler) GetExchangeURL() string {
	return b.config.AgreementBot.ExchangeURL
}

func (b *BaseConsumerProtocolHandler) GetCSSURL() string {
	return b.config.GetAgbotCSSURL()
}

func (b *BaseConsumerProtocolHandler) GetAgbotURL() string {
	return ""
}

func (b *BaseConsumerProtocolHandler) GetServiceBased() bool {
	return false
}

func (b *BaseConsumerProtocolHandler) GetHTTPFactory() *config.HTTPClientFactory {
	return b.config.Collaborators.HTTPClientFactory
}

func (w *BaseConsumerProtocolHandler) sendMessage(mt interface{}, pay []byte) error {
	// The mt parameter is an abstract message target object that is passed to this routine
	// by the agreement protocol. It's an interface{} type so that we can avoid the protocol knowing
	// about non protocol types.

	var messageTarget *exchange.ExchangeMessageTarget
	switch mt.(type) {
	case *exchange.ExchangeMessageTarget:
		messageTarget = mt.(*exchange.ExchangeMessageTarget)
	default:
		return errors.New(fmt.Sprintf("input message target is %T, expecting exchange.MessageTarget", mt))
	}

	logMsg := string(pay)

	// Try to demarshal pay into Proposal struct
	if glog.V(5) {
		if newProp, err := abstractprotocol.DemarshalProposal(logMsg); err == nil {
			// check if log message is a byte-encoded Proposal struct
			if len(newProp.AgreementId()) > 0 {
				// if it is a proposal, obscure the secrets
				if logMsg, err = abstractprotocol.ObscureProposalSecret(logMsg); err != nil {
					// something went wrong, send empty string to ensure secret protection
					logMsg = ""
				}
			}
		}
	}

	// Grab the exchange ID of the message receiver
	glog.V(3).Infof(BCPHlogstring(w.Name(), fmt.Sprintf("sending exchange message to: %v, message %v", messageTarget.ReceiverExchangeId, cutil.TruncateDisplayString(string(pay), 300))))
	if glog.V(5) {
		glog.Infof(BCPHlogstring(w.Name(), fmt.Sprintf("sending exchange message to: %v, message %v", messageTarget.ReceiverExchangeId, logMsg)))
	}

	// Get my own keys
	myPubKey, myPrivKey, keyErr := exchange.GetKeys(w.config.AgreementBot.MessageKeyPath)
	if keyErr != nil {
		return errors.New(fmt.Sprintf("error getting keys: %v", keyErr))
	}

	// Demarshal the receiver's public key if we need to
	if messageTarget.ReceiverPublicKeyObj == nil {
		if mtpk, err := exchange.DemarshalPublicKey(messageTarget.ReceiverPublicKeyBytes); err != nil {
			return errors.New(fmt.Sprintf("Unable to demarshal device's public key %x, error %v", messageTarget.ReceiverPublicKeyBytes, err))
		} else {
			messageTarget.ReceiverPublicKeyObj = mtpk
		}
	}

	exchDev, err := exchange.GetExchangeDevice(w.GetHTTPFactory(), messageTarget.ReceiverExchangeId, w.agbotId, w.token, w.config.AgreementBot.ExchangeURL)
	if err != nil {
		return fmt.Errorf("Unable to get device from exchange: %v", err)
	}
	maxHb := exchDev.HeartbeatIntv.MaxInterval
	if maxHb == 0 {
		exchOrg, err := exchange.GetOrganization(w.GetHTTPFactory(), exchange.GetOrg(messageTarget.ReceiverExchangeId), w.config.AgreementBot.ExchangeURL, w.agbotId, w.token)
		if err != nil {
			return fmt.Errorf("Unable to get org from exchange: %v", err)
		}
		maxHb = exchOrg.HeartbeatIntv.MaxInterval
	}
	exchangeMessageTTL := w.config.AgreementBot.GetExchangeMessageTTL(maxHb)

	// Create an encrypted message
	if encryptedMsg, err := exchange.ConstructExchangeMessage(pay, myPubKey, myPrivKey, messageTarget.ReceiverPublicKeyObj); err != nil {
		return errors.New(fmt.Sprintf("Unable to construct encrypted message, error %v for message %s", err, pay))
		// Marshal it into a byte array
	} else if msgBody, err := json.Marshal(encryptedMsg); err != nil {
		return errors.New(fmt.Sprintf("Unable to marshal exchange message, error %v for message %v", err, encryptedMsg))
		// Send it to the device's message queue
	} else {
		pm := exchange.CreatePostMessage(msgBody, exchangeMessageTTL)
		var resp interface{}
		resp = new(exchange.PostDeviceResponse)
		targetURL := w.config.AgreementBot.ExchangeURL + "orgs/" + exchange.GetOrg(messageTarget.ReceiverExchangeId) + "/nodes/" + exchange.GetId(messageTarget.ReceiverExchangeId) + "/msgs"
		for {
			if err, tpErr := exchange.InvokeExchange(w.httpClient, "POST", targetURL, w.agbotId, w.token, pm, &resp); err != nil {
				return err
			} else if tpErr != nil {
				glog.Warningf(tpErr.Error())
				time.Sleep(10 * time.Second)
				continue
			} else {
				if glog.V(5) {
					glog.Infof(BCPHlogstring(w.Name(), fmt.Sprintf("sent message for %v to exchange.", messageTarget.ReceiverExchangeId)))
				}
				return nil
			}
		}
	}

}

func (b *BaseConsumerProtocolHandler) DispatchProtocolMessage(cmd *NewProtocolMessageCommand, cph ConsumerProtocolHandler) error {

	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("received inbound exchange message.")))
	}

	// Figure out what kind of message this is
	if reply, rerr := cph.AgreementProtocolHandler("", "", "").ValidateReply(string(cmd.Message)); rerr == nil {
		agreementWork := NewHandleReply(reply, cmd.From, cmd.PubKey, cmd.MessageId)
		cph.WorkQueue().InboundHigh() <- &agreementWork
		if glog.V(5) {
			glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("queued reply message")))
		}
	} else if _, aerr := cph.AgreementProtocolHandler("", "", "").ValidateDataReceivedAck(string(cmd.Message)); aerr == nil {
		agreementWork := NewHandleDataReceivedAck(string(cmd.Message), cmd.From, cmd.PubKey, cmd.MessageId)
		cph.WorkQueue().InboundHigh() <- &agreementWork
		if glog.V(5) {
			glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("queued data received ack message")))
		}
	} else if can, cerr := cph.AgreementProtocolHandler("", "", "").ValidateCancel(string(cmd.Message)); cerr == nil {
		// Before dispatching the cancel to a worker thread, make sure it's a valid cancel
		if ag, err := b.db.FindSingleAgreementByAgreementId(can.AgreementId(), can.Protocol(), []persistence.AFilter{}); err != nil {
			glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error finding agreement %v in the db", can.AgreementId())))
		} else if ag == nil {
			glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("cancel ignored, cannot find agreement %v in the db", can.AgreementId())))
		} else if ag.DeviceId != cmd.From {
			glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("cancel ignored, cancel message for %v came from id %v but agreement is with %v", can.AgreementId(), cmd.From, ag.DeviceId)))
		} else {
			agreementWork := NewCancelAgreement(can.AgreementId(), can.Protocol(), can.Reason(), cmd.MessageId)
			cph.WorkQueue().InboundHigh() <- &agreementWork
			if glog.V(5) {
				glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("queued cancel message")))
			}
		}
	} else if exerr := cph.HandleExtensionMessage(cmd); exerr == nil {
		// nothing to do
	} else {
		if glog.V(5) {
			glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("ignoring  message: %v because it is an unknown type", string(cmd.Message))))
		}
		return errors.New(BCPHlogstring(b.Name(), fmt.Sprintf("unexpected protocol msg %v", cmd.Message)))
	}
	return nil

}

func (b *BaseConsumerProtocolHandler) HandleAgreementTimeout(cmd *AgreementTimeoutCommand, cph ConsumerProtocolHandler) {

	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "received agreement cancellation."))
	}
	agreementWork := NewCancelAgreement(cmd.AgreementId, cmd.Protocol, cmd.Reason, 0)
	cph.WorkQueue().InboundHigh() <- &agreementWork
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "queued agreement cancellation"))
	}

}

func (b *BaseConsumerProtocolHandler) HandlePolicyChanged(cmd *PolicyChangedCommand, cph ConsumerProtocolHandler) {

	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "received policy changed command."))
	}

	if eventPol, err := policy.DemarshalPolicy(cmd.Msg.PolicyString()); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error demarshalling change policy event %v, error: %v", cmd.Msg.PolicyString(), err)))
	} else {

		// Cancel related agreements
		InProgress := func() persistence.AFilter {
			return func(e persistence.Agreement) bool { return e.AgreementCreationTime != 0 && e.AgreementTimedout == 0 }
		}

		stillValidAgs := []string{}

		if agreements, err := b.db.FindAgreements([]persistence.AFilter{persistence.UnarchivedAFilter(), InProgress()}, cph.Name()); err == nil {
			for _, ag := range agreements {

				if pol, err := policy.DemarshalPolicy(ag.Policy); err != nil {
					glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("unable to demarshal policy for agreement %v, error %v", ag.CurrentAgreementId, err)))

				} else if eventPol.Header.Name != pol.Header.Name {
					// This agreement is using a policy different from the one that changed.
					if glog.V(5) {
						glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("policy change handler skipping agreement %v because it is using a policy that did not change.", ag.CurrentAgreementId)))
					}
					continue
				} else if err := b.pm.MatchesMine(cmd.Msg.Org(), pol); err != nil {
					if glog.V(5) {
						glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("cmd msg org matches mine for agreement %v", ag.CurrentAgreementId)))
					}
					agStillValid := false
					policyMatches := true
					noNewPriority := false
					clusterNSNotChange := true

					if ag.Pattern == "" {
						policyMatches, noNewPriority, clusterNSNotChange = b.HandlePolicyChangeForAgreement(ag, pol, cph)
						agStillValid = policyMatches && noNewPriority
						if ag.GetDeviceType() == persistence.DEVICE_TYPE_CLUSTER {
							agStillValid = agStillValid && clusterNSNotChange
						}
					}

					if glog.V(5) {
						glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("for current agreement %v: agStillValid: %v, policyMatches: %v, noNewPriority: %v, clusterNSNotChange: %v", ag.CurrentAgreementId, agStillValid, policyMatches, noNewPriority, clusterNSNotChange)))
					}

					if !agStillValid {
						glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("agreement %v has a policy %v that has changed incompatibly. Cancelling agreement: %v", ag.CurrentAgreementId, pol.Header.Name, err)))
						b.CancelAgreement(ag, TERM_REASON_POLICY_CHANGED, cph, policyMatches)
					} else {
						if glog.V(5) {
							glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("current agreement %v is still valid", ag.CurrentAgreementId)))
						}
						stillValidAgs = append(stillValidAgs, ag.CurrentAgreementId)
					}
				} else {
					if glog.V(5) {
						glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("for agreement %v, no policy content differences detected", ag.CurrentAgreementId)))
					}
				}

			}
		} else {
			glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error searching database: %v", err)))
		}

		AgNotKept := func(validAgs []string) persistence.WUFilter {
			return func(w persistence.WorkloadUsage) bool { return !cutil.SliceContains(validAgs, w.CurrentAgreementId) }
		}

		// Remove the workloadusage that has the same policy name and does not have the agreement id associated.
		// This will allow the highest serice version be tried under the new policy.
		// For the ones with the agreement id, the agreements will get canceled and the workload usage will be removed anyway.
		if wlu_array, err := b.db.FindWorkloadUsages([]persistence.WUFilter{persistence.PNoAWUFilter(eventPol.Header.Name), AgNotKept(stillValidAgs)}); err != nil {
			glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("Failed to get the workload usages with policy name: %v, %v", eventPol.Header.Name, err)))
		} else {
			for _, wlu := range wlu_array {
				if glog.V(5) {
					glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("deleting workload usage %v.", wlu)))
				}

				if err := b.db.DeleteWorkloadUsage(wlu.DeviceId, wlu.PolicyName); err != nil {
					glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("Failed to delete the workload usages with device id: %v, policy name: %v, %v", wlu.DeviceId, wlu.PolicyName, err)))
				}
			}
		}
	}
}

// first bool is true if the policy still matches, false otherwise
// second bool is true unless a higher priority workload than the current one has been added or changed
// third bool is true if the cluster namespace is not changed, this return value should be check only when device type is cluster
// if an error occurs, both will be false
func (b *BaseConsumerProtocolHandler) HandlePolicyChangeForAgreement(ag persistence.Agreement, oldPolicy *policy.Policy, cph ConsumerProtocolHandler) (bool, bool, bool) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("attempting to update agreement %v due to change in policy", ag.CurrentAgreementId)))
	}

	msgPrinter := i18n.GetMessagePrinter()

	svcAllPol := externalpolicy.ExternalPolicy{}
	svcPolicyHandler := exchange.GetHTTPServicePolicyHandler(b)
	svcResolveHandler := exchange.GetHTTPServiceDefResolverHandler(b)

	for _, svcId := range ag.ServiceId {
		if svcDef, err := exchange.GetServiceWithId(b, svcId); err != nil {
			glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("failed to get service %v, error: %v", svcId, err)))
			return false, false, false
		} else if svcDef != nil {
			if mergedSvcPol, _, _, _, _, err := compcheck.GetServicePolicyWithDefaultProperties(svcPolicyHandler, svcResolveHandler, svcDef.URL, exchange.GetOrg(svcId), svcDef.Version, svcDef.Arch, msgPrinter); err != nil {
				glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("failed to get merged service policy for %v, error: %v", svcId, err)))
				return false, false, false
			} else if mergedSvcPol != nil {
				svcAllPol.MergeWith(mergedSvcPol, false)
			}
		}
	}

	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("For agreement %v merged svc policy is %v", ag.CurrentAgreementId, svcAllPol)))
	}

	busPolHandler := exchange.GetHTTPBusinessPoliciesHandler(b)
	_, busPol, err := compcheck.GetBusinessPolicy(busPolHandler, ag.PolicyName, true, msgPrinter)
	if err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("failed to get business policy %v/%v from the exchange: %v", ag.Org, ag.PolicyName, err)))
		return false, false, false
	}

	nodePolHandler := exchange.GetHTTPNodePolicyHandler(b)
	_, nodePol, err := compcheck.GetNodePolicy(nodePolHandler, ag.DeviceId, msgPrinter)
	if err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("failed to get node policy for %v from the exchange.", ag.DeviceId)))
		return false, false, false
	}

	dev, err := exchange.GetExchangeDevice(b.GetHTTPFactory(), ag.DeviceId, b.GetExchangeId(), b.GetExchangeToken(), b.GetExchangeURL())
	if err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("failed to get node %v from the exchange.", ag.DeviceId)))
		return false, false, false
	} else if dev == nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("Device %v does not exist in the exchange.", ag.DeviceId)))
		return false, false, false
	}

	nodeArch := dev.Arch
	if canArch := b.config.ArchSynonyms.GetCanonicalArch(dev.Arch); canArch != "" {
		nodeArch = canArch
	}

	swVers, ok := dev.SoftwareVersions[exchange.AGENT_VERSION]
	if !ok {
		swVers = "0.0.0"
	}

	// skip for now if not all built-in properties are in the node policy
	// this will get called again after the node updates its policy with the built-ins
	if !externalpolicy.ContainsAllBuiltInNodeProps(&nodePol.Properties, swVers, dev.GetNodeType()) {
		return true, true, true
	}

	match, reason, producerPol, consumerPol, err := compcheck.CheckPolicyCompatiblility(nodePol, busPol, &svcAllPol, nodeArch, nil)

	if !match {
		glog.V(5).Infof(BCPHlogstring(b.Name(), fmt.Sprintf("agreement %v is not longer in policy. Reason is: %v", ag.CurrentAgreementId, reason)))
		return false, true, false
	}

	// don't send an update if the agreement is not finalized yet
	if ag.AgreementFinalizedTime == 0 {
		return true, true, true
	}

	// for every priority (in order highest to lowest) in the new policy with priority lower than the current wl
	// if it's not in the old policy, cancel
	choice := -1

	nextPriority := policy.GetNextWorkloadChoice(busPol.Workloads, choice)
	wl := nextPriority

	wlUsage, err := b.db.FindSingleWorkloadUsageByDeviceAndPolicyName(ag.DeviceId, ag.PolicyName)
	if err != nil {
		return false, false, false
	}
	// wlUsage is nil if no prioriy is set in the previous policy
	wlUsagePriority := 0
	if wlUsage != nil {
		wlUsagePriority = wlUsage.Priority
	}

	if currentWL := policy.GetWorkloadWithPriority(busPol.Workloads, wlUsagePriority); currentWL == nil {
		// the current workload priority is no longer in the deployment policy
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("current workload priority %v is no longer in policy for agreement %v", wlUsagePriority, ag.CurrentAgreementId)))
		return true, false, false
	} else {
		wl = currentWL
	}

	if oldPolicy != nil {
		for choice <= wlUsagePriority && nextPriority != nil {
			choice = nextPriority.Priority.PriorityValue
			matchingWL := policy.GetWorkloadWithPriority(oldPolicy.Workloads, choice)
			if matchingWL == nil || !matchingWL.IsSame(*nextPriority) {
				glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("Higher priority version added or modified. Cancelling agreement %v", ag.CurrentAgreementId)))
				return true, false, false
			}
			nextPriority = policy.GetNextWorkloadChoice(busPol.Workloads, choice)
		}

		// check if cluster namespace is changed in new policy
		if dev.NodeType == persistence.DEVICE_TYPE_CLUSTER && busPol.ClusterNamespace != oldPolicy.ClusterNamespace {
			if glog.V(5) {
				glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("cluster namespace is changed from %v to %v in busiess policy for agreement %v, checking cluster namespace compatibility ...", oldPolicy.ClusterNamespace, busPol.ClusterNamespace, ag.CurrentAgreementId)))
			}
			t_comp, consumerNamespace, t_reason := compcheck.CheckClusterNamespaceCompatibility(dev.NodeType, dev.ClusterNamespace, dev.IsNamespaceScoped, busPol.ClusterNamespace, wl.ClusterDeployment, ag.Pattern, false, msgPrinter)
			if !t_comp {
				if glog.V(5) {
					glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("cluster namespace %v is not longer compatible for agreement %v. Reason is: %v", consumerNamespace, ag.CurrentAgreementId, t_reason)))
				}
				return true, true, false
			} else if consumerNamespace != oldPolicy.ClusterNamespace {
				// this check only applies to cluster-scoped agent
				if glog.V(5) {
					glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("cluster namespace has changed from %v to %v for agreement %v", oldPolicy.ClusterNamespace, consumerNamespace, ag.CurrentAgreementId)))
				}
				return true, true, false
			}
			// cluster namespace remains same
		}
	}

	if wl.Arch == "" || wl.Arch == "*" {
		wl.Arch = nodeArch
	}

	// populate the workload with the deployment string
	if svcDef, _, err := exchange.GetHTTPServiceHandler(b)(wl.WorkloadURL, wl.Org, wl.Version, wl.Arch); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error getting service '%v' from the exchange, error: %v", wl, err)))
		return false, false, false
	} else if svcDef == nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("Service %v not found in the exchange.", wl)))
		return false, false, false
	} else {
		if dev.NodeType == persistence.DEVICE_TYPE_CLUSTER {
			wl.ClusterDeployment = svcDef.GetClusterDeploymentString()
			wl.ClusterDeploymentSignature = svcDef.GetClusterDeploymentSignature()
		} else {
			wl.Deployment = svcDef.GetDeploymentString()
			wl.DeploymentSignature = svcDef.GetDeploymentSignature()
		}
	}

	if same, msg := consumerPol.IsSamePolicy(oldPolicy); same {
		glog.V(3).Infof("business policy(producerPol) %v content remains same with old policy; no update to agreement %s", ag.PolicyName, ag.CurrentAgreementId)
		return true, true, true
	} else {
		glog.V(3).Infof("business policy %v content is changed in agreement %v: %v", ag.PolicyName, ag.CurrentAgreementId, msg)
	}

	newTsCs, err := policy.Create_Terms_And_Conditions(producerPol, consumerPol, wl, ag.CurrentAgreementId, b.config.AgreementBot.DefaultWorkloadPW, b.config.AgreementBot.NoDataIntervalS, basicprotocol.PROTOCOL_CURRENT_VERSION)
	if err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error creating new terms and conditions: %v", err)))
		return false, false, false
	}

	ag.LastPolicyUpdateTime = uint64(time.Now().Unix())

	// this function will send out "basicagreementupdate"
	b.UpdateAgreement(&ag, basicprotocol.MsgUpdateTypePolicyChange, newTsCs, cph)

	return true, true, true
}

func (b *BaseConsumerProtocolHandler) HandlePolicyDeleted(cmd *PolicyDeletedCommand, cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "received policy deleted command."))
	}

	// Remove the workloadusage that has the same policy name and does not have the agreement id associated.
	// For the ones with the agreement id, the agreements will get canceled and the workload usage will be removed anyway.
	if eventPol, err := policy.DemarshalPolicy(cmd.Msg.PolicyString()); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error demarshalling change policy event %v, error: %v", cmd.Msg.PolicyString(), err)))
	} else {
		if wlu_array, err := b.db.FindWorkloadUsages([]persistence.WUFilter{persistence.PNoAWUFilter(eventPol.Header.Name)}); err != nil {
			glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("Failed to get the workload usages with policy name: %v, %v", eventPol.Header.Name, err)))
		} else {
			for _, wlu := range wlu_array {
				if glog.V(5) {
					glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("deleting workload usage %v.", wlu)))
				}

				if err := b.db.DeleteWorkloadUsage(wlu.DeviceId, wlu.PolicyName); err != nil {
					glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("Failed to delete the workload usages with device id: %v, policy name: %v, %v", wlu.DeviceId, wlu.PolicyName, err)))
				}
			}
		}
	}

	InProgress := func() persistence.AFilter {
		return func(e persistence.Agreement) bool { return e.AgreementCreationTime != 0 && e.AgreementTimedout == 0 }
	}

	if agreements, err := b.db.FindAgreements([]persistence.AFilter{persistence.UnarchivedAFilter(), InProgress()}, cph.Name()); err == nil {
		for _, ag := range agreements {

			if pol, err := policy.DemarshalPolicy(ag.Policy); err != nil {
				glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("unable to demarshal policy for agreement %v, error %v", ag.CurrentAgreementId, err)))
			} else if cmd.Msg.Org() == ag.Org {
				if existingPol := b.pm.GetPolicy(cmd.Msg.Org(), pol.Header.Name); existingPol == nil {
					glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("agreement %v has a policy %v that doesn't exist anymore", ag.CurrentAgreementId, pol.Header.Name)))

					// Remove any workload usage records so that a new agreement will be made starting from the highest priority workload.
					if err := b.db.DeleteWorkloadUsage(ag.DeviceId, ag.PolicyName); err != nil {
						glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("error deleting workload usage for %v using policy %v, error: %v", ag.DeviceId, ag.PolicyName, err)))
					}

					// Queue up a cancellation command for this agreement.
					agreementWork := NewCancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, cph.GetTerminationCode(TERM_REASON_POLICY_CHANGED), 0)
					cph.WorkQueue().InboundHigh() <- &agreementWork

				}
			}
		}
	} else {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error searching database: %v", err)))
	}
}

func (b *BaseConsumerProtocolHandler) HandleServicePolicyChanged(cmd *ServicePolicyChangedCommand, cph ConsumerProtocolHandler) {

	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "received service policy changed command: %v. cmd"))
	}

	InProgress := func() persistence.AFilter {
		return func(e persistence.Agreement) bool { return e.AgreementCreationTime != 0 && e.AgreementTimedout == 0 }
	}

	if agreements, err := b.db.FindAgreements([]persistence.AFilter{persistence.UnarchivedAFilter(), InProgress()}, cph.Name()); err == nil {
		for _, ag := range agreements {
			if ag.Pattern == "" && ag.PolicyName == fmt.Sprintf("%v/%v", cmd.Msg.BusinessPolOrg, cmd.Msg.BusinessPolName) && ag.ServiceId[0] == cmd.Msg.ServiceId {
				policyMatches, noNewPriority, _ := b.HandlePolicyChangeForAgreement(ag, nil, cph)
				agStillValid := policyMatches && noNewPriority
				if !agStillValid {
					glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("agreement %v has a service policy %v that has changed.", ag.CurrentAgreementId, ag.ServiceId)))
					b.CancelAgreement(ag, TERM_REASON_POLICY_CHANGED, cph, policyMatches)
				}
			}
		}
	} else {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error searching database: %v", err)))
	}
}

func (b *BaseConsumerProtocolHandler) HandleNodePolicyChanged(cmd *NodePolicyChangedCommand, cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "recieved node policy change command."))
	}

	InProgress := func() persistence.AFilter {
		return func(e persistence.Agreement) bool { return e.AgreementCreationTime != 0 && e.AgreementTimedout == 0 }
	}

	if agreements, err := b.db.FindAgreements([]persistence.AFilter{persistence.UnarchivedAFilter(), InProgress()}, cph.Name()); err == nil {
		for _, ag := range agreements {
			if ag.Pattern == "" && ag.DeviceId == cutil.FormOrgSpecUrl(cmd.Msg.NodeId, cmd.Msg.NodePolOrg) {
				policyMatches, noNewPriority, _ := b.HandlePolicyChangeForAgreement(ag, nil, cph)
				agStillValid := policyMatches && noNewPriority
				if !agStillValid {
					glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("agreement %v has a node policy %v that has changed.", ag.CurrentAgreementId, ag.ServiceId)))
					b.CancelAgreement(ag, TERM_REASON_POLICY_CHANGED, cph, policyMatches)
				} else {
					// If the agreement is still valid, then handlePolicyChangeFor MMS object
					b.HandlePolicyChangeForMMSObject(ag, cph)
				}
			}
		}
	} else {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error searching database: %v", err)))
	}
}

func (b *BaseConsumerProtocolHandler) HandleServicePolicyDeleted(cmd *ServicePolicyDeletedCommand, cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "received policy deleted command."))
	}

	InProgress := func() persistence.AFilter {
		return func(e persistence.Agreement) bool { return e.AgreementCreationTime != 0 && e.AgreementTimedout == 0 }
	}

	if agreements, err := b.db.FindAgreements([]persistence.AFilter{persistence.UnarchivedAFilter(), InProgress()}, cph.Name()); err == nil {
		for _, ag := range agreements {
			if ag.Pattern == "" && ag.PolicyName == fmt.Sprintf("%v/%v", cmd.Msg.BusinessPolOrg, cmd.Msg.BusinessPolName) && ag.ServiceId[0] == cmd.Msg.ServiceId {
				glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("agreement %v has a service policy %v that doesn't exist anymore", ag.CurrentAgreementId, ag.ServiceId)))

				// Remove any workload usage records so that a new agreement will be made starting from the highest priority workload.
				if err := b.db.DeleteWorkloadUsage(ag.DeviceId, ag.PolicyName); err != nil {
					glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("error deleting workload usage for %v using policy %v, error: %v", ag.DeviceId, ag.PolicyName, err)))
				}

				// Queue up a cancellation command for this agreement.
				agreementWork := NewCancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, cph.GetTerminationCode(TERM_REASON_POLICY_CHANGED), 0)
				cph.WorkQueue().InboundHigh() <- &agreementWork

			}
		}
	} else {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error searching database: %v", err)))
	}
}

func (b *BaseConsumerProtocolHandler) HandleMMSObjectPolicy(cmd *MMSObjectPolicyEventCommand, cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("received object policy change command.")))
	}
	agreementWork := NewObjectPolicyChange(cmd.Msg)
	cph.WorkQueue().InboundHigh() <- &agreementWork
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("queued object policy change command.")))
	}
}

// HandlePolicyChangeForMMSObject need to:
// 1. for each service in agreement, grab object policy using service info
// 2. AssignObjectToNode func (nodePlicy, objectPolicy ...), then add/delete destination
func (b *BaseConsumerProtocolHandler) HandlePolicyChangeForMMSObject(agreement persistence.Agreement, cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), "handle node policy change for MMS object."))
	}

	if agreement.GetDeviceType() == persistence.DEVICE_TYPE_DEVICE {
		if b.GetCSSURL() != "" && agreement.Pattern == "" {
			AgreementHandleMMSObjectPolicy(b, b.mmsObjMgr, agreement, b.Name(), BCPHlogstring)
		} else if b.GetCSSURL() == "" {
			glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("unable to re-evaluate object placement because there is no CSS URL configured in this agbot")))
		}
	}
}

// Note: Multiple agbot could call this function at the same time (for different agreement).
//
//	Table workloadusage is partitioned. So one agbot could only see the workloadusage in
//	its own partition. Table ha_workload_upgrade is not partitioned.
func (b *BaseConsumerProtocolHandler) CancelAgreement(ag persistence.Agreement, reason string, cph ConsumerProtocolHandler, policyMatches bool) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("Canceling Agreement: %v, reason: %v", ag, reason)))
	}
	// Remove any workload usage records (non-HA) or mark for pending upgrade (HA). There might not be a workload usage record
	// if the consumer policy does not specify the workload priority section.
	if wlUsage, err := b.db.FindSingleWorkloadUsageByDeviceAndPolicyName(ag.DeviceId, ag.PolicyName); err != nil {
		glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("error retreiving workload usage for %v using policy %v, error: %v", ag.DeviceId, ag.PolicyName, err)))
	} else if wlUsage != nil && policyMatches {
		theDev, err := GetDevice(b.config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), ag.DeviceId, b.config.AgreementBot.ExchangeURL, cph.GetExchangeId(), cph.GetExchangeToken())
		if err != nil {
			glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error getting device %v, error: %v", ag.DeviceId, err)))
			return
		}

		if theDev != nil && theDev.HAGroup != "" {
			// update pending upgrade for itself. So that governerHA won't think this device has finish upgrade
			if glog.V(5) {
				glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("setting workloadusage pending for %v using policy %v", ag.DeviceId, ag.PolicyName)))
			}
			if _, err := b.db.UpdatePendingUpgrade(ag.DeviceId, ag.PolicyName); err != nil {
				glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("unable to set workloadusage pending for %v using policy %v, error: %v", ag.DeviceId, ag.PolicyName, err)))
			}

			deviceAndGroupOrg := exchange.GetOrg(ag.DeviceId)
			if upgradingWorkload, err := b.db.GetHAUpgradingWorkload(deviceAndGroupOrg, theDev.HAGroup, ag.PolicyName); err != nil {
				glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error get HA upgrading workload with hagroup %v, org: %v, policyName: %v, error: %v", theDev.HAGroup, ag.Org, ag.PolicyName, err)))
				return
			} else if upgradingWorkload != nil {
				// there is a upgrading workload, let the govenance handle the status and order
				if glog.V(5) {
					glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("upgrading workload: %v", upgradingWorkload)))
				}
				return
			}

			// put this workload in HA workload upgrading table
			if glog.V(5) {
				glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("inserting HA upgrading workloads with hagroup %v, org: %v, policyName: %v deviceId: %v", theDev.HAGroup, ag.Org, ag.PolicyName, ag.DeviceId)))
			}
			if currentNodeId, err := b.db.InsertHAUpgradingWorkloadForGroupAndPolicy(deviceAndGroupOrg, theDev.HAGroup, ag.PolicyName, ag.DeviceId); err != nil {
				glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("unable to insert HA upgrading workloads with hagroup %v, org: %v, policyName: %v deviceId: %v, error: %v", theDev.HAGroup, ag.Org, ag.PolicyName, ag.DeviceId, err)))
				return
			} else if currentNodeId == ag.DeviceId {
				if glog.V(5) {
					glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("delete workloadusage and cancel agreement for: org: %v, hagroup: %v, policyName: %v deviceId: %v", ag.Org, theDev.HAGroup, ag.PolicyName, ag.DeviceId)))
				}
				if err := b.db.DeleteWorkloadUsage(ag.DeviceId, ag.PolicyName); err != nil {
					glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("error deleting workload usage for %v using policy %v, error: %v", ag.DeviceId, ag.PolicyName, err)))
				}
				agreementWork := NewCancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, cph.GetTerminationCode(reason), 0)
				cph.WorkQueue().InboundHigh() <- &agreementWork
				return
			} else {
				glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("unable to insert HA upgrading workloads with hagroup %v, org: %v, policyName: %v deviceId: %v because there is another node %v exists already in the table.", theDev.HAGroup, ag.Org, ag.PolicyName, ag.DeviceId, currentNodeId)))
				return
			}
		}
	}

	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("delete non-HA workloadusage and cancel agreement for: org: %v, policyName: %v deviceId: %v", ag.Org, ag.PolicyName, ag.DeviceId)))
	}
	// reach here when it is a non-HA workload:
	// 1) wlUsage == nil
	// 2) theDev == nil || theDev.HAGroup == ""
	// Non-HA device or agreement without workload priority in the policy, re-make the agreement.
	// Delete this workload usage record so that a new agreement will be made starting from the highest priority workload
	if err := b.db.DeleteWorkloadUsage(ag.DeviceId, ag.PolicyName); err != nil {
		glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("error deleting workload usage for %v using policy %v, error: %v", ag.DeviceId, ag.PolicyName, err)))
	}
	agreementWork := NewCancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, cph.GetTerminationCode(reason), 0)
	cph.WorkQueue().InboundHigh() <- &agreementWork
}

func (b *BaseConsumerProtocolHandler) HandleWorkloadUpgrade(cmd *WorkloadUpgradeCommand, cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("received workload upgrade command.")))
	}
	upgradeWork := NewHandleWorkloadUpgrade(cmd.Msg.AgreementId, cmd.Msg.AgreementProtocol, cmd.Msg.DeviceId, cmd.Msg.PolicyName)
	cph.WorkQueue().InboundHigh() <- &upgradeWork
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("queued workload upgrade command.")))
	}
}

func (b *BaseConsumerProtocolHandler) HandleMakeAgreement(cmd *MakeAgreementCommand, cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("received make agreement command.")))
	}
	agreementWork := NewInitiateAgreement(cmd.ProducerPolicy, cmd.ConsumerPolicy, cmd.Org, cmd.Device, cmd.ConsumerPolicyName, cmd.ServicePolicies)
	cph.WorkQueue().InboundLow() <- &agreementWork
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("queued make agreement command.")))
	}
}

func (b *BaseConsumerProtocolHandler) HandleStopProtocol(cph ConsumerProtocolHandler) {
	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("received stop protocol command.")))
	}

	for ix := 0; ix < b.config.AgreementBot.AgreementWorkers; ix++ {
		work := NewStopWorker()
		cph.WorkQueue().InboundHigh() <- &work
	}

	if glog.V(5) {
		glog.Infof(BCPHlogstring(b.Name(), fmt.Sprintf("queued %x stop protocol commands.", b.config.AgreementBot.AgreementWorkers)))
	}
}

func (b *BaseConsumerProtocolHandler) PersistBaseAgreement(wi *InitiateAgreement, proposal abstractprotocol.Proposal, workerID string, hash string, sig string) error {

	if polBytes, err := json.Marshal(wi.ConsumerPolicy); err != nil {
		return errors.New(BCPHlogstring2(workerID, fmt.Sprintf("error marshalling policy for storage %v, error: %v", wi.ConsumerPolicy, err)))
	} else if pBytes, err := json.Marshal(proposal); err != nil {
		return errors.New(BCPHlogstring2(workerID, fmt.Sprintf("error marshalling proposal for storage %v, error: %v", proposal, err)))
	} else if pol, err := policy.DemarshalPolicy(proposal.TsAndCs()); err != nil {
		return errors.New(BCPHlogstring2(workerID, fmt.Sprintf("error demarshalling TsandCs policy from pending agreement %v, error: %v", proposal.AgreementId(), err)))
	} else if _, err := b.db.AgreementUpdate(proposal.AgreementId(), string(pBytes), string(polBytes), pol.DataVerify, b.config.AgreementBot.ProcessGovernanceIntervalS, hash, sig, b.Name(), proposal.Version()); err != nil {
		return errors.New(BCPHlogstring2(workerID, fmt.Sprintf("error updating agreement with proposal %v in DB, error: %v", proposal, err)))

		// Record that the agreement was initiated, in the exchange
	} else if err := b.RecordConsumerAgreementState(proposal.AgreementId(), pol, wi.Org, "Formed Proposal", workerID); err != nil {
		return errors.New(BCPHlogstring2(workerID, fmt.Sprintf("error setting agreement state for %v", proposal.AgreementId())))
	}

	return nil
}

func (b *BaseConsumerProtocolHandler) PersistReply(reply abstractprotocol.ProposalReply, pol *policy.Policy, workerID string) error {

	if _, err := b.db.AgreementMade(reply.AgreementId(), reply.DeviceId(), "", b.Name(), "", "", ""); err != nil {
		return errors.New(BCPHlogstring2(workerID, fmt.Sprintf("error updating agreement %v with reply info in DB, error: %v", reply.AgreementId(), err)))
	}
	return nil
}

func (b *BaseConsumerProtocolHandler) RecordConsumerAgreementState(agreementId string, pol *policy.Policy, org string, state string, workerID string) error {

	workload := pol.Workloads[0].WorkloadURL

	if glog.V(5) {
		glog.Infof(BCPHlogstring2(workerID, fmt.Sprintf("setting agreement %v for workload %v/%v state to %v", agreementId, org, workload, state)))
	}
	as := new(exchange.PutAgbotAgreementState)
	as.Service = exchange.WorkloadAgreement{
		Org:     org,
		Pattern: exchange.GetId(pol.PatternId),
		URL:     workload,
	}
	as.State = state

	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := b.config.AgreementBot.ExchangeURL + "orgs/" + exchange.GetOrg(b.agbotId) + "/agbots/" + exchange.GetId(b.agbotId) + "/agreements/" + agreementId
	for {
		if err, tpErr := exchange.InvokeExchange(b.httpClient, "PUT", targetURL, b.agbotId, b.token, &as, &resp); err != nil {
			glog.Errorf(err.Error())
			return err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			if glog.V(5) {
				glog.Infof(BCPHlogstring2(workerID, fmt.Sprintf("set agreement %v to state %v", agreementId, state)))
			}
			return nil
		}
	}

}

func (b *BaseConsumerProtocolHandler) DeleteMessage(msgId int) error {

	return DeleteMessage(msgId, b.agbotId, b.token, b.config.AgreementBot.ExchangeURL, b.httpClient)

}

func (b *BaseConsumerProtocolHandler) TerminateAgreement(ag *persistence.Agreement, reason uint, mt interface{}, workerId string, cph ConsumerProtocolHandler) {
	if pol, err := policy.DemarshalPolicy(ag.Policy); err != nil {
		glog.Errorf(BCPHlogstring2(workerId, fmt.Sprintf("unable to demarshal policy while trying to cancel %v, error %v", ag.CurrentAgreementId, err)))
	} else {
		bcType, bcName, bcOrg := cph.GetKnownBlockchain(ag)
		if aph := cph.AgreementProtocolHandler(bcType, bcName, bcOrg); aph == nil {
			glog.Warningf(BCPHlogstring2(workerId, fmt.Sprintf("for %v agreement protocol handler not ready", ag.CurrentAgreementId)))
		} else if err := aph.TerminateAgreement([]policy.Policy{*pol}, ag.CounterPartyAddress, ag.CurrentAgreementId, ag.Org, reason, mt, b.GetSendMessage()); err != nil {
			glog.Errorf(BCPHlogstring2(workerId, fmt.Sprintf("error terminating agreement %v: %v", ag.CurrentAgreementId, err)))
		}
	}
}

func (b *BaseConsumerProtocolHandler) VerifyAgreement(ag *persistence.Agreement, cph ConsumerProtocolHandler) {

	if aph := cph.AgreementProtocolHandler(b.GetKnownBlockchain(ag)); aph == nil {
		glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("for %v agreement protocol handler not ready", ag.CurrentAgreementId)))
	} else if whisperTo, pubkeyTo, err := b.GetDeviceMessageEndpoint(ag.DeviceId, b.Name()); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error obtaining message target for verify message: %v", err)))
	} else if mt, err := exchange.CreateMessageTarget(ag.DeviceId, nil, pubkeyTo, whisperTo); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error creating message target: %v", err)))
	} else if _, err := aph.VerifyAgreement(ag.CurrentAgreementId, "", "", mt, b.GetSendMessage()); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error verifying agreement %v: %v", ag.CurrentAgreementId, err)))
	}

}

func (b *BaseConsumerProtocolHandler) UpdateAgreement(ag *persistence.Agreement, updateType string, metadata interface{}, cph ConsumerProtocolHandler) {

	if aph := cph.AgreementProtocolHandler(b.GetKnownBlockchain(ag)); aph == nil {
		glog.Warningf(BCPHlogstring(b.Name(), fmt.Sprintf("for %v agreement protocol handler not ready", ag.CurrentAgreementId)))
	} else if whisperTo, pubkeyTo, err := b.GetDeviceMessageEndpoint(ag.DeviceId, b.Name()); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error obtaining message target for verify message: %v", err)))
	} else if mt, err := exchange.CreateMessageTarget(ag.DeviceId, nil, pubkeyTo, whisperTo); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error creating message target: %v", err)))
	} else if err := aph.UpdateAgreement(ag.CurrentAgreementId, updateType, metadata, mt, b.GetSendMessage()); err != nil {
		glog.Errorf(BCPHlogstring(b.Name(), fmt.Sprintf("error updating agreement %v: %v", ag.CurrentAgreementId, err)))
	}

}

func (b *BaseConsumerProtocolHandler) GetDeviceMessageEndpoint(deviceId string, workerId string) (string, []byte, error) {

	if glog.V(5) {
		glog.Infof(BCPHlogstring2(workerId, fmt.Sprintf("retrieving device %v msg endpoint from exchange", deviceId)))
	}

	if dev, err := b.getDevice(deviceId, workerId); err != nil {
		return "", nil, err
	} else if publicKeyBytes, err := base64.StdEncoding.DecodeString(dev.PublicKey); err != nil {
		return "", nil, errors.New(fmt.Sprintf("Error decoding device publicKey for %s, %v", deviceId, err))
	} else {
		if glog.V(5) {
			glog.Infof(BCPHlogstring2(workerId, fmt.Sprintf("retrieved device %v msg endpoint from exchange %v", deviceId, dev.MsgEndPoint)))
		}
		return dev.MsgEndPoint, publicKeyBytes, nil
	}

}

func (b *BaseConsumerProtocolHandler) getDevice(deviceId string, workerId string) (*exchange.Device, error) {

	if glog.V(5) {
		glog.Infof(BCPHlogstring2(workerId, fmt.Sprintf("retrieving device %v from exchange", deviceId)))
	}

	var resp interface{}
	resp = new(exchange.GetDevicesResponse)
	targetURL := b.config.AgreementBot.ExchangeURL + "orgs/" + exchange.GetOrg(deviceId) + "/nodes/" + exchange.GetId(deviceId)
	for {
		if err, tpErr := exchange.InvokeExchange(b.config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), "GET", targetURL, b.agbotId, b.token, nil, &resp); err != nil {
			glog.Errorf(BCPHlogstring2(workerId, fmt.Sprintf(err.Error())))
			return nil, err
		} else if tpErr != nil {
			glog.Warningf(BCPHlogstring2(workerId, tpErr.Error()))
			time.Sleep(10 * time.Second)
			continue
		} else {
			devs := resp.(*exchange.GetDevicesResponse).Devices
			if dev, there := devs[deviceId]; !there {
				return nil, errors.New(fmt.Sprintf("device %v not in GET response %v as expected", deviceId, devs))
			} else {
				if glog.V(5) {
					glog.Infof(BCPHlogstring2(workerId, fmt.Sprintf("retrieved device %v from exchange %v", deviceId, dev)))
				}
				return &dev, nil
			}
		}
	}
}

func (b *BaseConsumerProtocolHandler) DeferCommand(cmd AgreementWork) {
	b.deferredCommands = append(b.deferredCommands, cmd)
}

func (b *BaseConsumerProtocolHandler) GetDeferredCommands() []AgreementWork {
	res := b.deferredCommands
	b.deferredCommands = make([]AgreementWork, 0, 10)
	return res
}

func (b *BaseConsumerProtocolHandler) UpdateProducer(ag *persistence.Agreement) {
	return
}

func (b *BaseConsumerProtocolHandler) HandleExtensionMessage(cmd *NewProtocolMessageCommand) error {
	return nil
}

func (c *BaseConsumerProtocolHandler) SetBlockchainClientAvailable(ev *events.BlockchainClientInitializedMessage) {
	return
}

func (c *BaseConsumerProtocolHandler) SetBlockchainClientNotAvailable(ev *events.BlockchainClientStoppingMessage) {
	return
}

func (c *BaseConsumerProtocolHandler) AlreadyReceivedReply(ag *persistence.Agreement) bool {
	if ag.CounterPartyAddress != "" {
		return true
	}
	return false
}

func (c *BaseConsumerProtocolHandler) GetKnownBlockchain(ag *persistence.Agreement) (string, string, string) {
	return "", "", ""
}

func (c *BaseConsumerProtocolHandler) CanSendMeterRecord(ag *persistence.Agreement) bool {
	return true
}

func (b *BaseConsumerProtocolHandler) SendEventMessage(event events.Message) {
	if len(b.messages) < int(b.config.GetAgbotAgreementQueueSize()) {
		b.messages <- event
	}
}

// The list of termination reasons that should be supported by all agreement protocols. The caller can pass these into
// the GetTerminationCode API to get a protocol specific reason code for that termination reason.
const TERM_REASON_POLICY_CHANGED = "PolicyChanged"
const TERM_REASON_NOT_FINALIZED_TIMEOUT = "NotFinalized"
const TERM_REASON_NO_DATA_RECEIVED = "NoData"
const TERM_REASON_NO_REPLY = "NoReply"
const TERM_REASON_USER_REQUESTED = "UserRequested"
const TERM_REASON_DEVICE_REQUESTED = "DeviceRequested"
const TERM_REASON_NEGATIVE_REPLY = "NegativeReply"
const TERM_REASON_CANCEL_DISCOVERED = "CancelDiscovered"
const TERM_REASON_CANCEL_FORCED_UPGRADE = "ForceUpgrade"
const TERM_REASON_CANCEL_BC_WRITE_FAILED = "WriteFailed"
const TERM_REASON_NODE_HEARTBEAT = "NodeHeartbeat"
const TERM_REASON_AG_MISSING = "AgreementMissing"

var BCPHlogstring = func(p string, v interface{}) string {
	return fmt.Sprintf("Base Consumer Protocol Handler (%v) %v", p, v)
}

var BCPHlogstring2 = func(workerID string, v interface{}) string {
	return fmt.Sprintf("Base Consumer Protocol Handler (%v): %v", workerID, v)
}
