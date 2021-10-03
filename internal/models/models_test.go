package models

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type ModelsTestSuite struct {
	suite.Suite
}

func (s *ModelsTestSuite) TestColumnsBuild() {
	assert.Equal(s.T(), reflect.TypeOf(Atom{}).NumField(), len(AtomColumns))
	assert.Equal(s.T(), reflect.TypeOf(Callback{}).NumField(), len(CallbackColumns))
	assert.Equal(s.T(), reflect.TypeOf(Job{}).NumField(), len(JobColumns))
	assert.Equal(s.T(), reflect.TypeOf(Task{}).NumField(), len(TaskColumns))
	assert.Equal(s.T(), reflect.TypeOf(Trigger{}).NumField(), len(TriggerColumns))
}

func TestModelsTestSuite(t *testing.T) {
	suite.Run(t, new(ModelsTestSuite))
}
