// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"math"
	"reflect"
	"time"

	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/pkg/errors"
	"github.com/splitio/go-client/splitio/client"
	"github.com/splitio/go-client/splitio/conf"
)

// setupFeatureFlags called on startup and when the cluster leader changes.
// starts or stops the syncronization of feature flags from upstream managment.
func (s *Server) setupFeatureFlags() {
	license := s.License()
	inCloud := license != nil && *license.Features.Cloud
	splitKey := *s.Config().ServiceSettings.SplitKey
	syncFeatureFlags := inCloud && splitKey != "" && s.IsLeader()

	if inCloud {
		s.configStore.PersistFeatures(true)
	} else {
		s.configStore.PersistFeatures(false)
	}

	if syncFeatureFlags {
		if err := s.startFeatureFlagUpdateJob(); err != nil {
			s.Log.Error("Unable to setup synchronization with feature flag management. Will fallback to cloud cache.", mlog.Err(err))
		}
	} else {
		s.stopFeatureFlagUpdateJob()
	}
}

func (s *Server) updateFeatureFlagValuesFromManagment() {
	newCfg := s.Config().Clone()
	oldFlags := *newCfg.FeatureFlags
	newFlags := s.featureFlagSynchronizer.updateFeatureFlagValues(oldFlags)
	if oldFlags != newFlags {
		*newCfg.FeatureFlags = newFlags
		s.SaveConfig(newCfg, true)
	}
}

func (s *Server) startFeatureFlagUpdateJob() error {
	// Can be run multiple times
	if s.featureFlagSynchronizer != nil {
		return nil
	}

	var log *mlog.Logger
	if *s.Config().ServiceSettings.DebugSplit {
		log = s.Log
	}

	syncronizer, err := newFeatureFlagSynchronizer(FeatureFlagSyncParams{
		ServerID:            s.TelemetryId(),
		SplitKey:            *s.Config().ServiceSettings.SplitKey,
		SyncIntervalSeconds: *s.Config().ServiceSettings.FeatureFlagSyncIntervalSeconds,
		Log:                 log,
	})
	if err != nil {
		return err
	}

	s.featureFlagStop = make(chan struct{})
	s.featureFlagStopped = make(chan struct{})
	s.featureFlagSynchronizer = syncronizer

	go func() {
		defer close(s.featureFlagStopped)
		if err := syncronizer.ensureReady(); err != nil {
			s.Log.Error("Problem connecting to feature flag managment. Will fallback to cloud cache.", mlog.Err(err))
			return
		}
		s.updateFeatureFlagValuesFromManagment()
		for {
			select {
			case <-s.featureFlagStop:
				return
			case <-time.After(time.Duration(10) * time.Second):
				s.updateFeatureFlagValuesFromManagment()
			}
		}
	}()

	return nil
}

func (s *Server) stopFeatureFlagUpdateJob() {
	if s.featureFlagSynchronizer != nil {
		close(s.featureFlagStop)
		<-s.featureFlagStopped
		s.featureFlagSynchronizer.close()
		s.featureFlagSynchronizer = nil
	}
}

type FeatureFlagSyncParams struct {
	ServerID            string
	SplitKey            string
	SyncIntervalSeconds int
	Log                 *mlog.Logger
}

type featureFlagSynchronizer struct {
	FeatureFlagSyncParams

	client  *client.SplitClient
	stop    chan struct{}
	stopped chan struct{}
}

func newFeatureFlagSynchronizer(params FeatureFlagSyncParams) (*featureFlagSynchronizer, error) {
	cfg := conf.Default()
	if params.Log != nil {
		cfg.Logger = &splitMlogAdapter{wrappedLog: params.Log.With(mlog.String("service", "split"))}
	} else {
		cfg.LoggerConfig.LogLevel = math.MinInt32
	}
	factory, err := client.NewSplitFactory(params.SplitKey, cfg)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create split factory")
	}

	return &featureFlagSynchronizer{
		FeatureFlagSyncParams: params,
		client:                factory.Client(),
		stop:                  make(chan struct{}),
		stopped:               make(chan struct{}),
	}, nil
}

// ensureReady blocks until the syncronizer is ready to update feature flag values
func (f *featureFlagSynchronizer) ensureReady() error {
	if err := f.client.BlockUntilReady(10); err != nil {
		return errors.Wrap(err, "split.io client could not initalize")
	}

	return nil
}

func (f *featureFlagSynchronizer) updateFeatureFlagValues(base model.FeatureFlags) model.FeatureFlags {
	featureNames := getStructFields(model.FeatureFlags{})
	featuresMap := f.client.Treatments(f.ServerID, featureNames, nil)
	ffm := featureFlagsFromMap(featuresMap, base)
	return ffm
}

func (f *featureFlagSynchronizer) close() {
	f.client.Destroy()
}

// featureFlagsFromMap sets the feature flags from a map[string]string.
// It starts with baseFeatureFlags and only sets values that are
// given by the upstream managment system.
// Makes the assumption that all feature flags are strings for now.
func featureFlagsFromMap(featuresMap map[string]string, baseFeatureFlags model.FeatureFlags) model.FeatureFlags {
	refStruct := reflect.ValueOf(&baseFeatureFlags).Elem()
	for fieldName, fieldValue := range featuresMap {
		refField := refStruct.FieldByName(fieldName)
		if !refField.IsValid() || !refField.CanSet() {
			continue
		}

		refField.Set(reflect.ValueOf(fieldValue))
	}
	return baseFeatureFlags
}

func getStructFields(s interface{}) []string {
	structType := reflect.TypeOf(s)
	fieldNames := make([]string, 0, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		fieldNames = append(fieldNames, structType.Field(i).Name)
	}

	return fieldNames
}
