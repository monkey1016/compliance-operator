package controller

import (
	"github.com/openshift/compliance-operator/pkg/controller/tailoredprofile"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, tailoredprofile.Add)
}
