package controller

import (
	"github.com/openshift/compliance-operator/pkg/controller/profilebundle"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, profilebundle.Add)
}
