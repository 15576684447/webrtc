package ice_support_remonination

import (
	"testing"
)

func Test_candidateBase_Equal(t *testing.T) {
	obj1 := &candidateBase{id: "test1"}
	obj2 := &CandidateHost{*obj1, "test2"}
	obj2.id = "test2"

	if !obj1.Equal(obj2) {
		t.Errorf("candidateBase.Equal() = %v, want %v", obj2, obj1)
	}

	obj2.relatedAddress = &CandidateRelatedAddress{"test", 1}
	if !obj1.Equal(obj2) {
		t.Errorf("candidateBase.Equal() = %v, want %v", obj2, obj1)
	}
}
