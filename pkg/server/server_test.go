package server

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/bizflycloud/bizfly-backup/pkg/backupapi"
	"github.com/bizflycloud/bizfly-backup/pkg/broker"
	"github.com/bizflycloud/bizfly-backup/pkg/broker/mqtt"
	"github.com/bizflycloud/bizfly-backup/pkg/cache"
	"github.com/bizflycloud/bizfly-backup/pkg/storage_vault"

	"github.com/go-chi/chi"
	"github.com/ory/dockertest/v3"
	"github.com/panjf2000/ants/v2"
	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"
)

const (
	defaultTestPort = 9000
)

var (
	b       broker.Broker
	topic   = "agent/agent1"
	mqttURL string
)

func TestMain(m *testing.M) {
	if os.Getenv("EXCLUDE_MQTT") != "" {
		os.Exit(0)
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}

	resource, err := pool.Run("vernemq/vernemq", "latest-alpine", []string{"DOCKER_VERNEMQ_USER_foo=bar", "DOCKER_VERNEMQ_ACCEPT_EULA=yes"})
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}

	mqttURL = fmt.Sprintf("mqtt://foo:bar@%s", resource.GetHostPort("1883/tcp"))
	if err := pool.Retry(func() error {
		var err error
		b, err = mqtt.NewBroker(mqtt.WithURL(mqttURL), mqtt.WithClientID("sub"))
		if err != nil {
			return err
		}
		return b.Connect()
	}); err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}

	code := m.Run()

	if err := pool.Purge(resource); err != nil {
		log.Fatalf("Could not purge resource: %s", err)
	}
	os.Exit(code)
}

func TestServerRun(t *testing.T) {
	tests := []struct {
		addr string
	}{
		{"http://localhost:" + strconv.Itoa(defaultTestPort)},
	}
	for _, tc := range tests {
		s, err := New(WithAddr(tc.addr), WithBroker(b))
		require.NoError(t, err)
		s.testSignalCh = make(chan os.Signal, 1)
		var serverError error
		done := make(chan struct{})
		go func() {
			serverError = s.Run()
			close(done)
		}()
		time.Sleep(time.Duration(rand.Intn(1000)) * time.Millisecond)
		s.testSignalCh <- syscall.SIGTERM
		<-done
		assert.IsType(t, http.ErrServerClosed, serverError)
	}
}

func TestServerEventHandler(t *testing.T) {
	addr := "http://localhost:" + strconv.Itoa(defaultTestPort)
	s, err := New(WithAddr(addr), WithBroker(b))
	require.NoError(t, err)

	done := make(chan struct{})
	stop := make(chan struct{})
	count := 0

	go func() {
		require.NoError(t, s.b.Subscribe([]string{topic}, func(e broker.Event) error {
			count++
			if count == 2 {
				close(stop)
			}
			return errors.New("unknown event)")
		}))
		close(done)
	}()

	<-done
	pub, err := mqtt.NewBroker(mqtt.WithURL(mqttURL), mqtt.WithClientID("pub"))
	require.NoError(t, err)
	require.NotNil(t, pub)
	assert.NoError(t, pub.Connect())
	assert.NoError(t, pub.Publish(topic, `{"event_type": "test"`))
	assert.NoError(t, pub.Publish(topic, `{"event_type": ""`))
	<-stop
	assert.Equal(t, 2, count)
}

func TestServerCron(t *testing.T) {
	tests := []struct {
		name               string
		bdc                []backupapi.BackupDirectoryConfig
		expectedNumEntries int
	}{
		{
			"empty",
			[]backupapi.BackupDirectoryConfig{},
			0,
		},
		{
			"good",
			[]backupapi.BackupDirectoryConfig{
				{
					ID:   "dir1",
					Name: "dir1",
					Path: "/dev/null",
					Policies: []backupapi.BackupDirectoryConfigPolicy{
						{
							ID:              "policy_1",
							Name:            "policy_1",
							SchedulePattern: "* * * * *",
						},
					},
					Activated: true,
				},
				{
					ID:   "dir2",
					Name: "dir2",
					Path: "/dev/zero",
					Policies: []backupapi.BackupDirectoryConfigPolicy{
						{
							ID:              "policy_2",
							Name:            "policy_2",
							SchedulePattern: "* * * * *",
						},
					},
					Activated: true,
				},
			},
			2,
		},
		{
			"activated false",
			[]backupapi.BackupDirectoryConfig{
				{
					ID:   "dir1",
					Name: "dir1",
					Path: "/dev/null",
					Policies: []backupapi.BackupDirectoryConfigPolicy{
						{
							ID:              "policy_1",
							Name:            "policy_1",
							SchedulePattern: "* * * * *",
						},
					},
					Activated: true,
				},
				{
					ID:   "dir2",
					Name: "dir2",
					Path: "/dev/zero",
					Policies: []backupapi.BackupDirectoryConfigPolicy{
						{
							ID:              "policy_2",
							Name:            "policy_2",
							SchedulePattern: "* * * * *",
						},
					},
					Activated: false,
				},
			},
			1,
		},
		{
			"invalid cron pattern",
			[]backupapi.BackupDirectoryConfig{
				{
					ID:   "dir1",
					Name: "dir1",
					Path: "/dev/null",
					Policies: []backupapi.BackupDirectoryConfigPolicy{
						{
							ID:              "policy_1",
							Name:            "policy_1",
							SchedulePattern: "* * * *",
						},
					},
					Activated: true,
				},
			},
			0,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := New()
			require.NoError(t, err)
			s.addToCronManager(tc.bdc)
			assert.Len(t, s.mappingToCronEntryID, tc.expectedNumEntries)
			s.removeFromCronManager(tc.bdc)
			assert.Equal(t, map[string]cron.EntryID{}, s.mappingToCronEntryID)
		})
	}
}

func TestServer_storeFiles(t *testing.T) {
	type fields struct {
		Addr                 string
		router               *chi.Mux
		b                    broker.Broker
		subscribeTopics      []string
		publishTopics        []string
		useUnixSock          bool
		backupClient         *backupapi.Client
		cronManager          *cron.Cron
		mappingToCronEntryID map[string]cron.EntryID
		testSignalCh         chan os.Signal
		poolDir              *ants.Pool
		pool                 *ants.Pool
		chunkPool            *ants.Pool
		logger               *zap.Logger
	}
	type args struct {
		cachePath    string
		mcID         string
		rpID         string
		index        *cache.Index
		storageVault storage_vault.StorageVault
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "test witer file csv",
			fields: fields{
				Addr: "http://localhost:" + strconv.Itoa(defaultTestPort),
			},
			args: args{
				cachePath: "cache",
				mcID:      "1",
				rpID:      "1",
				index: &cache.Index{
					BackupDirectoryID: "1",
					RecoveryPointID:   "1",
					TotalFiles:        1,
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				Addr:                 tt.fields.Addr,
				router:               tt.fields.router,
				b:                    tt.fields.b,
				subscribeTopics:      tt.fields.subscribeTopics,
				publishTopics:        tt.fields.publishTopics,
				useUnixSock:          tt.fields.useUnixSock,
				backupClient:         tt.fields.backupClient,
				cronManager:          tt.fields.cronManager,
				mappingToCronEntryID: tt.fields.mappingToCronEntryID,
				testSignalCh:         tt.fields.testSignalCh,
				poolDir:              tt.fields.poolDir,
				pool:                 tt.fields.pool,
				chunkPool:            tt.fields.chunkPool,
				logger:               tt.fields.logger,
			}
			if err := s.storeFiles(tt.args.cachePath, tt.args.mcID, tt.args.rpID, tt.args.index, tt.args.storageVault); (err != nil) != tt.wantErr {
				t.Errorf("Server.writeFileCSV() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
