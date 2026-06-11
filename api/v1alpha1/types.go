package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "guard.3istor.com", Version: "v1alpha1"}

type TargetRef struct {
	Name string `json:"name"`
}

type OffhoursScheduleSpec struct {
	TargetRef TargetRef `json:"targetRef"`
	SleepAt   string    `json:"sleepAt"`
	WakeAt    string    `json:"wakeAt"`
	Timezone  string    `json:"timezone,omitempty"`
}

type OffhoursScheduleStatus struct {
	CurrentState     string `json:"currentState,omitempty"`
	OriginalReplicas int32  `json:"originalReplicas,omitempty"`
}

type OffhoursSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OffhoursScheduleSpec   `json:"spec,omitempty"`
	Status OffhoursScheduleStatus `json:"status,omitempty"`
}

type OffhoursScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OffhoursSchedule `json:"items"`
}

func (in *OffhoursSchedule) DeepCopyObject() runtime.Object {
	out := *in
	return &out
}

func (in *OffhoursScheduleList) DeepCopyObject() runtime.Object {
	out := *in
	return &out
}