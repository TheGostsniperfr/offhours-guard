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

type TargetStatus struct {
	Name             string `json:"name"`
	OriginalReplicas int32  `json:"originalReplicas"`
}

type OffhoursScheduleSpec struct {
	TargetRefs []TargetRef `json:"targetRefs"`
	SleepAt    string      `json:"sleepAt"`
	WakeAt     string      `json:"wakeAt"`
	Timezone   string      `json:"timezone,omitempty"`
}

type OffhoursScheduleStatus struct {
	CurrentState   string         `json:"currentState,omitempty"`
	TargetStatuses []TargetStatus `json:"targetStatuses,omitempty"`
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
	if in.Spec.TargetRefs != nil {
		out.Spec.TargetRefs = make([]TargetRef, len(in.Spec.TargetRefs))
		copy(out.Spec.TargetRefs, in.Spec.TargetRefs)
	}
	if in.Status.TargetStatuses != nil {
		out.Status.TargetStatuses = make([]TargetStatus, len(in.Status.TargetStatuses))
		copy(out.Status.TargetStatuses, in.Status.TargetStatuses)
	}
	return &out
}

func (in *OffhoursScheduleList) DeepCopyObject() runtime.Object {
	out := *in
	return &out
}