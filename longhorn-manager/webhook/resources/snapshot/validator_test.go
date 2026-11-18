package snapshot

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/client-go/kubernetes/fake"

	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/longhorn/longhorn-manager/datastore"
	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	lhfake "github.com/longhorn/longhorn-manager/k8s/pkg/client/clientset/versioned/fake"
)

const testNamespace = "longhorn-system"

// newTestDataStore returns a datastore whose volume lister serves the given
// volumes, as if the informer had already delivered them.
func newTestDataStore(t *testing.T, volumes ...*longhorn.Volume) *datastore.DataStore {
	kubeClient := fake.NewSimpleClientset()                    // nolint: staticcheck
	lhClient := lhfake.NewSimpleClientset()                    // nolint: staticcheck
	extensionsClient := apiextensionsfake.NewSimpleClientset() // nolint: staticcheck

	informerFactories := util.NewInformerFactories(testNamespace, kubeClient, lhClient, 0)
	ds := datastore.NewDataStore(testNamespace, lhClient, kubeClient, extensionsClient, informerFactories)

	volumeIndexer := informerFactories.LhInformerFactory.Longhorn().V1beta2().Volumes().Informer().GetIndexer()
	for _, volume := range volumes {
		require.NoError(t, volumeIndexer.Add(volume))
	}
	return ds
}

// TestSnapshotValidatorDelete verifies that deleting a snapshot is only denied
// when it is the entrypoint of a linked-clone volume, and that snapshot names
// that cannot be used as a label value (longer than 63 bytes) do not block the
// deletion.
func TestSnapshotValidatorDelete(t *testing.T) {
	linkedCloneLabelKey := types.GetLonghornLabelKey(types.LonghornLabelLinkedCloneSourceSnapshot)
	longSnapshotName := "backup-snapshot-0000000-pvc-00000000-0000-0000-0000-000000000000"
	require.Len(t, longSnapshotName, 64)

	newSnapshot := func(name string) *longhorn.Snapshot {
		return &longhorn.Snapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec:       longhorn.SnapshotSpec{Volume: "vol-1"},
		}
	}
	newVolume := func(name string, labels map[string]string) *longhorn.Volume {
		return &longhorn.Volume{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		}
	}

	testCases := []struct {
		name         string
		snapshotName string
		volumes      []*longhorn.Volume
		expectErrMsg string
	}{
		{
			name:         "snapshot not referenced by any linked-clone volume is allowed",
			snapshotName: "snap-1",
			volumes:      []*longhorn.Volume{newVolume("vol-1", nil)},
		},
		{
			name:         "linked-clone entrypoint snapshot is rejected",
			snapshotName: "snap-1",
			volumes: []*longhorn.Volume{
				newVolume("vol-1", nil),
				newVolume("clone-b", map[string]string{linkedCloneLabelKey: "snap-1"}),
				newVolume("clone-a", map[string]string{linkedCloneLabelKey: "snap-1"}),
			},
			expectErrMsg: "cannot delete snapshot snap-1: it is the entrypoint for linked-clone volume(s): clone-a, clone-b",
		},
		{
			name:         "snapshot name of exactly 63 bytes is allowed",
			snapshotName: strings.Repeat("a", 63),
			volumes:      []*longhorn.Volume{newVolume("vol-1", nil)},
		},
		{
			name:         "snapshot name longer than 63 bytes is allowed",
			snapshotName: longSnapshotName,
			volumes:      []*longhorn.Volume{newVolume("vol-1", nil)},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			v := &snapshotValidator{ds: newTestDataStore(t, tc.volumes...)}
			err := v.Delete(nil, newSnapshot(tc.snapshotName))
			if tc.expectErrMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectErrMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
