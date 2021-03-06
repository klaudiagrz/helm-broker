package broker_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"testing"
	"time"

	osb "github.com/kubernetes-sigs/go-open-service-broker-client/v2"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"

	"github.com/kyma-project/helm-broker/internal/platform/ptr"

	"github.com/kyma-project/helm-broker/internal"
	"github.com/kyma-project/helm-broker/internal/bind"
	"github.com/kyma-project/helm-broker/internal/broker"
	"github.com/kyma-project/helm-broker/internal/broker/automock"
	"github.com/kyma-project/helm-broker/internal/platform/logger/spy"
	"github.com/kyma-project/helm-broker/internal/storage"
	"helm.sh/helm/v3/pkg/release"
)

func newOSBAPITestSuite(t *testing.T) *osbapiTestSuite {
	logSink := spy.NewLogSink()
	logSink.RawLogger.Out = ioutil.Discard

	sFact, err := storage.NewFactory(storage.NewConfigListAllMemory())
	require.NoError(t, err)

	ts := &osbapiTestSuite{
		t:              t,
		StorageFactory: sFact,
		HelmClient:     &automock.HelmClient{},
		LogSink:        logSink,
	}

	ts.Exp.Populate()

	ts.OperationIDProvider = func() (internal.OperationID, error) {
		return ts.Exp.OperationID, nil
	}

	ts.BrokerServer = broker.NewWithIDProvider(
		sFact.Addon(),
		sFact.Chart(),
		sFact.InstanceOperation(),
		sFact.BindOperation(),
		sFact.Instance(),
		sFact.InstanceBindData(),
		&fakeBindTmplRenderer{},
		&fakeBindTmplResolver{},
		ts.HelmClient,
		logSink.Logger, ts.OperationIDProvider)

	return ts
}

type osbapiTestSuite struct {
	t *testing.T

	BrokerServer        *broker.Server
	StorageFactory      storage.Factory
	HelmClient          *automock.HelmClient
	LogSink             *spy.LogSink
	OperationIDProvider func() (internal.OperationID, error)

	osbClient osb.Client

	serverWg     sync.WaitGroup
	serverCancel func()
	ServerAddr   string

	Exp expAll
}

func (ts *osbapiTestSuite) ServerRun() {
	ctx, cancel := context.WithCancel(context.Background())
	ts.serverWg.Add(1)

	startedCh := make(chan struct{})

	go func() {
		assert.Equal(ts.t, http.ErrServerClosed, ts.BrokerServer.Run(ctx, ":0", startedCh))
		ts.serverWg.Done()
	}()

	// TODO: wrap in timeout
	<-startedCh
	ts.ServerAddr = ts.BrokerServer.Addr()
	ts.serverCancel = cancel
}

func (ts *osbapiTestSuite) ServerShutdown() {
	ts.serverCancel()
	ts.serverWg.Wait()
}

func (ts *osbapiTestSuite) OSBClient() osb.Client {
	if ts.osbClient == nil {
		config := osb.DefaultClientConfiguration()
		config.URL = fmt.Sprintf("http://%s/cluster", ts.ServerAddr)
		config.EnableAlphaFeatures = true

		osbClient, err := osb.NewClient(config)
		require.NoError(ts.t, err)
		ts.osbClient = osbClient
	}

	return ts.osbClient
}

const testNs = "test"

func (ts *osbapiTestSuite) OSBClientNS() osb.Client {
	if ts.osbClient == nil {
		config := osb.DefaultClientConfiguration()
		config.URL = fmt.Sprintf("http://%s/ns/%s", ts.ServerAddr, testNs)
		config.EnableAlphaFeatures = true

		osbClient, err := osb.NewClient(config)
		require.NoError(ts.t, err)
		ts.osbClient = osbClient
	}

	return ts.osbClient
}

func (ts *osbapiTestSuite) AssertOperationState(exp internal.OperationState) bool {

	doCheck := func() bool {
		op, err := ts.StorageFactory.InstanceOperation().Get(ts.Exp.InstanceID, ts.Exp.OperationID)
		require.NoError(ts.t, err)
		if op.State == exp {
			return true
		}
		return false
	}

	if doCheck() {
		return true
	}

	timeoutTotal := time.After(time.Second)
Polling:
	for {
		select {
		case <-timeoutTotal:
			ts.t.Error("timeout on instance operation state change")
			break Polling
		case <-time.After(time.Millisecond):
		}

		if doCheck() {
			return true
		}
	}

	return false
}

func (ts *osbapiTestSuite) AssertBindOperationState(exp internal.OperationState) bool {

	doCheck := func() bool {
		op, err := ts.StorageFactory.BindOperation().Get(ts.Exp.InstanceID, ts.Exp.BindingID, ts.Exp.OperationID)
		require.NoError(ts.t, err)
		if op.State == exp {
			return true
		}
		return false
	}

	if doCheck() {
		return true
	}

	timeoutTotal := time.After(time.Second)
Polling:
	for {
		select {
		case <-timeoutTotal:
			ts.t.Error("timeout on bind operation state change")
			break Polling
		case <-time.After(time.Millisecond):
		}

		if doCheck() {
			return true
		}
	}

	return false
}

func TestOSBAPICatalogSuccess(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	_, err := ts.StorageFactory.Addon().Upsert(internal.ClusterWide, fixAddon)
	require.NoError(t, err)

	// WHEN
	_, err = ts.OSBClient().GetCatalog()

	// THEN
	require.NoError(t, err)
}

func TestOSBAPIProvisionSuccess(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	ts.HelmClient.On("Install", mock.Anything, mock.Anything, ts.Exp.ReleaseName, ts.Exp.Namespace).Return(&release.Release{
		Info: &release.Info{},
	}, nil).Once()
	defer ts.HelmClient.AssertExpectations(t)

	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	_, err := ts.StorageFactory.Addon().Upsert(internal.ClusterWide, fixAddon)
	require.NoError(t, err)

	fixChart := ts.Exp.NewChart()
	ts.StorageFactory.Chart().Upsert(internal.ClusterWide, fixChart)

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClient().ProvisionInstance(req)

	// THEN
	require.NoError(t, err)

	require.True(t, resp.Async)
	assert.EqualValues(t, ts.Exp.OperationID, *resp.OperationKey)

	ts.AssertOperationState(internal.OperationStateSucceeded)
}

func TestOSBAPIProvisionRepeatedOnAlreadyFullyProvisionedInstance(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters:          ts.Exp.ProvisioningParameters.Data,
	}

	// WHEN
	resp, err := ts.OSBClient().ProvisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIProvisionRepeatedOnProvisioningInProgress(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	expOpID := internal.OperationID("fix-op-id")
	fixOperation.OperationID = expOpID
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClient().ProvisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.True(t, resp.Async)
	assert.EqualValues(t, expOpID, *resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIProvisionConflictErrorOnAlreadyFullyProvisionedInstance(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters:          ts.Exp.RequestProvisioningParameters,
	}

	// WHEN
	resp, err := ts.OSBClient().ProvisionInstance(req)

	//THEN
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.Async)

	//GIVEN Instance with conflict
	req = &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters:          ts.Exp.ProvisioningParameters.Data,
	}

	// WHEN
	resp, err = ts.OSBClient().ProvisionInstance(req)

	// THEN
	assert.Nil(t, resp)
	assert.Equal(t, osb.HTTPStatusCodeError{StatusCode: http.StatusConflict, ErrorMessage: ptrStr(fmt.Sprintf("service instance exists with different parameters: %v", ts.Exp.ProvisioningParameters.Data)), Description: ptrStr("")}, err)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionOnAlreadyDeprovisionedInstance(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeRemove, internal.OperationStateSucceeded)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClient().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionOnAlreadyDeprovisionedAndRemovedInstance(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	// storage does not contain any data

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClient().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionRepeatedOnDeprovisioningInProgress(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeRemove, internal.OperationStateInProgress)
	expOpID := internal.OperationID("fix-op-id")
	fixOperation.OperationID = expOpID
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClient().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.True(t, resp.Async)
	assert.EqualValues(t, expOpID, *resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionSuccess(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	expOpID := internal.OperationID("fix-op-id")
	fixOperation.OperationID = expOpID
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.HelmClient.On("Delete", ts.Exp.ReleaseName, ts.Exp.Namespace).Return(nil).Once()
	defer ts.HelmClient.AssertExpectations(t)

	ts.ServerRun()
	defer ts.ServerShutdown()

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClient().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	require.True(t, resp.Async)
	assert.EqualValues(t, ts.Exp.OperationID, *resp.OperationKey)

	ts.AssertOperationState(internal.OperationStateSucceeded)
}

func TestOSBAPILastOperationSuccess(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	// WHEN
	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.LastOperationRequest{
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}
	resp, err := ts.OSBClient().PollLastOperation(req)

	// THEN
	require.NoError(t, err)
	assert.EqualValues(t, internal.OperationStateInProgress, resp.State)
	// TODO: match desc
}

func TestOSBAPILastOperationForNonExistingInstance(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	// WHEN
	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.LastOperationRequest{
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}
	_, err := ts.OSBClient().PollLastOperation(req)

	// THEN
	assert.True(t, osb.IsGoneError(err))
}

func TestOSBAPIBindFailureWithDisallowedParametersFieldInReq(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	_, err := ts.StorageFactory.Addon().Upsert(internal.ClusterWide, fixAddon)
	require.NoError(t, err)

	// WHEN
	req := &osb.BindRequest{
		AcceptsIncomplete: true,
		BindingID:         "bind-id",
		InstanceID:        "instance-id",
		ServiceID:         "svc-id",
		PlanID:            "bind-id",
		Parameters: map[string]interface{}{
			"params": "set-but-not-allowed",
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}
	_, err = ts.OSBClient().Bind(req)

	// THEN
	require.Error(t, err)
	castedErr, ok := osb.IsHTTPError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, castedErr.StatusCode)
}

func TestOSBAPIBindSuccess(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)

	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	_, err := ts.StorageFactory.Addon().Upsert(internal.ClusterWide, fixAddon)
	require.NoError(t, err)

	fixChart := ts.Exp.NewChart()
	ts.StorageFactory.Chart().Upsert(internal.ClusterWide, fixChart)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Upsert(fixInstance)

	req := &osb.BindRequest{
		AcceptsIncomplete: true,
		BindingID:         string(ts.Exp.BindingID),
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClient().Bind(req)

	// then
	require.NoError(t, err)

	require.True(t, resp.Async)
	assert.EqualValues(t, ts.Exp.OperationID, *resp.OperationKey)

	ts.AssertBindOperationState(internal.OperationStateSucceeded)
}

func TestOSBAPIBindRepeatedOnAlreadyExistingBinding(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Upsert(fixInstance)

	fixAddon := ts.Exp.NewAddon()
	_, err := ts.StorageFactory.Addon().Upsert(internal.ClusterWide, fixAddon)
	require.NoError(t, err)

	fixChart := ts.Exp.NewChart()
	ts.StorageFactory.Chart().Upsert(internal.ClusterWide, fixChart)

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	fixCreds := *ts.Exp.NewInstanceCredentials()
	fixIbd := ts.Exp.NewInstanceBindData(fixCreds)
	ts.StorageFactory.InstanceBindData().Insert(fixIbd)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.BindRequest{
		BindingID:         string(ts.Exp.BindingID),
		InstanceID:        string(ts.Exp.InstanceID),
		AcceptsIncomplete: true,
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClient().Bind(req)

	// then
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)
	assert.EqualValues(t, map[string]interface{}{
		"password": "secret",
	}, resp.Credentials)
}

func TestOSBAPIBindRepeatedOnBindingInProgress(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.BindRequest{
		BindingID:         string(ts.Exp.BindingID),
		InstanceID:        string(ts.Exp.InstanceID),
		AcceptsIncomplete: true,
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClient().Bind(req)

	// then
	require.NoError(t, err)

	assert.True(t, resp.Async)
	assert.EqualValues(t, fixOperation.OperationID, *resp.OperationKey)
}

func TestOSBAPIBindingLastOperationSuccess(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.BindingLastOperationRequest{
		InstanceID:          string(ts.Exp.InstanceID),
		BindingID:           string(ts.Exp.BindingID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClient().PollBindingLastOperation(req)

	// then
	require.NoError(t, err)
	assert.EqualValues(t, internal.OperationStateInProgress, resp.State)

}

func TestOSBAPIBindingLastOperationFailure(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.BindingLastOperationRequest{
		InstanceID:          "",
		BindingID:           string(ts.Exp.BindingID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClient().PollBindingLastOperation(req)

	// then
	require.Error(t, err)
	assert.Nil(t, resp)

}

func TestOSBAPIBindFailureWithDisallowedParametersFieldInReqNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	ts.StorageFactory.Addon().Upsert(testNs, fixAddon)

	// WHEN
	req := &osb.BindRequest{
		AcceptsIncomplete: true,
		BindingID:         "bind-id",
		InstanceID:        "instance-id",
		ServiceID:         "svc-id",
		PlanID:            "bind-id",
		Parameters: map[string]interface{}{
			"params": "set-but-not-allowed",
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}
	_, err := ts.OSBClientNS().Bind(req)

	// THEN
	require.Error(t, err)
	castedErr, ok := osb.IsHTTPError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, castedErr.StatusCode)
}

func TestOSBAPICatalogSuccessNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	ts.StorageFactory.Addon().Upsert(testNs, fixAddon)

	// WHEN
	_, err := ts.OSBClientNS().GetCatalog()

	// THEN
	require.NoError(t, err)
}

func TestOSBAPIProvisionSuccessNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	ts.HelmClient.On("Install", mock.Anything, mock.Anything, ts.Exp.ReleaseName, ts.Exp.Namespace).Return(&release.Release{Info: &release.Info{}}, nil).Once()
	defer ts.HelmClient.AssertExpectations(t)

	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	ts.StorageFactory.Addon().Upsert(testNs, fixAddon)

	fixChart := ts.Exp.NewChart()
	ts.StorageFactory.Chart().Upsert(testNs, fixChart)

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters:          ts.Exp.ProvisioningParameters.Data,
	}

	// WHEN
	resp, err := ts.OSBClientNS().ProvisionInstance(req)

	// THEN
	require.NoError(t, err)

	require.True(t, resp.Async)
	assert.EqualValues(t, ts.Exp.OperationID, *resp.OperationKey)

	ts.AssertOperationState(internal.OperationStateSucceeded)
}

func TestOSBAPIProvisionRepeatedOnAlreadyFullyProvisionedInstanceNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters:          ts.Exp.ProvisioningParameters.Data,
	}

	// WHEN
	resp, err := ts.OSBClientNS().ProvisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIProvisionRepeatedOnProvisioningInProgressNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	expOpID := internal.OperationID("fix-op-id")
	fixOperation.OperationID = expOpID
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters: map[string]interface{}{
			"sample-parameter": "sample-value",
		},
	}

	// WHEN
	resp, err := ts.OSBClientNS().ProvisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.True(t, resp.Async)
	assert.EqualValues(t, expOpID, *resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIProvisionConflictErrorOnAlreadyFullyProvisionedInstanceNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	nsUID := uuid.NewRandom().String()
	req := &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters:          ts.Exp.RequestProvisioningParameters,
	}

	// WHEN
	resp, err := ts.OSBClientNS().ProvisionInstance(req)

	// THEN
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.Async)

	//GIVEN Instance with conflict
	req = &osb.ProvisionRequest{
		AcceptsIncomplete: true,
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OrganizationGUID:    nsUID,
		SpaceGUID:           nsUID,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
		Parameters:          ts.Exp.ProvisioningParameters.Data,
	}

	// WHEN
	resp, err = ts.OSBClient().ProvisionInstance(req)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionOnAlreadyDeprovisionedInstanceNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeRemove, internal.OperationStateSucceeded)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClientNS().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionOnAlreadyDeprovisionedAndRemovedInstanceNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	// storage does not contain any data

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClientNS().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionRepeatedOnDeprovisioningInProgressNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeRemove, internal.OperationStateInProgress)
	expOpID := internal.OperationID("fix-op-id")
	fixOperation.OperationID = expOpID
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClientNS().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	assert.True(t, resp.Async)
	assert.EqualValues(t, expOpID, *resp.OperationKey)

	// No activity should happen
	defer ts.HelmClient.AssertExpectations(t)
}

func TestOSBAPIDeprovisionSuccessNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	expOpID := internal.OperationID("fix-op-id")
	fixOperation.OperationID = expOpID
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	ts.HelmClient.On("Delete", ts.Exp.ReleaseName, ts.Exp.Namespace).Return(nil).Once()
	defer ts.HelmClient.AssertExpectations(t)

	ts.ServerRun()
	defer ts.ServerShutdown()

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	req := &osb.DeprovisionRequest{
		AcceptsIncomplete:   true,
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           string(ts.Exp.Service.ID),
		PlanID:              string(ts.Exp.ServicePlan.ID),
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// WHEN
	resp, err := ts.OSBClientNS().DeprovisionInstance(req)

	// THEN
	require.NoError(t, err)

	require.True(t, resp.Async)
	assert.EqualValues(t, ts.Exp.OperationID, *resp.OperationKey)

	ts.AssertOperationState(internal.OperationStateSucceeded)
}

func TestOSBAPILastOperationSuccessNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixOperation := ts.Exp.NewInstanceOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.InstanceOperation().Insert(fixOperation)

	// WHEN
	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.LastOperationRequest{
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}
	resp, err := ts.OSBClientNS().PollLastOperation(req)

	// THEN
	require.NoError(t, err)
	assert.EqualValues(t, internal.OperationStateInProgress, resp.State)
	// TODO: match desc
}

func TestOSBAPILastOperationForNonExistingInstanceNS(t *testing.T) {
	// GIVEN
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	// WHEN
	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.LastOperationRequest{
		InstanceID:          string(ts.Exp.InstanceID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}
	_, err := ts.OSBClientNS().PollLastOperation(req)

	// THEN
	assert.True(t, osb.IsGoneError(err))
}

func TestOSBAPIBindSuccessNS(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)

	ts.ServerRun()
	defer ts.ServerShutdown()

	fixAddon := ts.Exp.NewAddon()
	ts.StorageFactory.Addon().Upsert(testNs, fixAddon)

	fixChart := ts.Exp.NewChart()
	ts.StorageFactory.Chart().Upsert(testNs, fixChart)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Upsert(fixInstance)

	req := &osb.BindRequest{
		AcceptsIncomplete: true,
		BindingID:         string(ts.Exp.BindingID),
		InstanceID:        string(ts.Exp.InstanceID),
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClientNS().Bind(req)

	// then
	require.NoError(t, err)

	require.True(t, resp.Async)
	assert.EqualValues(t, ts.Exp.OperationID, *resp.OperationKey)

	ts.AssertBindOperationState(internal.OperationStateSucceeded)
}

func TestOSBAPIBindRepeatedOnAlreadyExistingBindingNS(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixAddon := ts.Exp.NewAddon()
	ts.StorageFactory.Addon().Upsert(testNs, fixAddon)

	fixChart := ts.Exp.NewChart()
	ts.StorageFactory.Chart().Upsert(testNs, fixChart)

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateSucceeded)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	fixCreds := *ts.Exp.NewInstanceCredentials()
	fixIbd := ts.Exp.NewInstanceBindData(fixCreds)
	ts.StorageFactory.InstanceBindData().Insert(fixIbd)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.BindRequest{
		BindingID:         string(ts.Exp.BindingID),
		InstanceID:        string(ts.Exp.InstanceID),
		AcceptsIncomplete: true,
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClientNS().Bind(req)

	// then
	require.NoError(t, err)

	assert.False(t, resp.Async)
	assert.Nil(t, resp.OperationKey)
	assert.EqualValues(t, map[string]interface{}{
		"password": "secret",
	}, resp.Credentials)
}

func TestOSBAPIBindRepeatedOnBindingInProgressNS(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)

	fixInstance := ts.Exp.NewInstance()
	ts.StorageFactory.Instance().Insert(fixInstance)

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	ts.ServerRun()
	defer ts.ServerShutdown()

	req := &osb.BindRequest{
		BindingID:         string(ts.Exp.BindingID),
		InstanceID:        string(ts.Exp.InstanceID),
		AcceptsIncomplete: true,
		ServiceID:         string(ts.Exp.Service.ID),
		PlanID:            string(ts.Exp.ServicePlan.ID),
		Context: map[string]interface{}{
			"namespace": string(ts.Exp.Namespace),
		},
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClientNS().Bind(req)

	// then
	require.NoError(t, err)

	assert.True(t, resp.Async)
	assert.EqualValues(t, fixOperation.OperationID, *resp.OperationKey)
}

func TestOSBAPIBindingLastOperationSuccessNS(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.BindingLastOperationRequest{
		InstanceID:          string(ts.Exp.InstanceID),
		BindingID:           string(ts.Exp.BindingID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClientNS().PollBindingLastOperation(req)

	// then
	require.NoError(t, err)
	assert.EqualValues(t, internal.OperationStateInProgress, resp.State)

}

func TestOSBAPIBindingLastOperationFailureNS(t *testing.T) {
	// given
	ts := newOSBAPITestSuite(t)
	ts.ServerRun()
	defer ts.ServerShutdown()

	fixOperation := ts.Exp.NewBindOperation(internal.OperationTypeCreate, internal.OperationStateInProgress)
	ts.StorageFactory.BindOperation().Insert(fixOperation)

	opKey := osb.OperationKey(ts.Exp.OperationID)
	req := &osb.BindingLastOperationRequest{
		InstanceID:          "",
		BindingID:           string(ts.Exp.BindingID),
		ServiceID:           ptr.String(string(ts.Exp.Service.ID)),
		PlanID:              ptr.String(string(ts.Exp.ServicePlan.ID)),
		OperationKey:        &opKey,
		OriginatingIdentity: &osb.OriginatingIdentity{Platform: osb.PlatformKubernetes, Value: "{}"},
	}

	// when
	resp, err := ts.OSBClientNS().PollBindingLastOperation(req)

	// then
	require.Error(t, err)
	assert.Nil(t, resp)

}

type fakeBindTmplRenderer struct{}

func (fakeBindTmplRenderer) Render(bindTemplate internal.AddonPlanBindTemplate, instance *internal.Instance, chart *chart.Chart) (bind.RenderedBindYAML, error) {
	return []byte(`fake`), nil
}

type fakeBindTmplResolver struct{}

func (fakeBindTmplResolver) Resolve(bindYAML bind.RenderedBindYAML, ns internal.Namespace) (*bind.ResolveOutput, error) {
	return &bind.ResolveOutput{}, nil
}

func ptrStr(str string) *string {
	return &str
}
