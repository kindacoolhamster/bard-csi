package inspect

import (
	"encoding/json"
	"fmt"
)

// The decoders below turn raw Kubernetes list JSON into the package's minimal
// structs. They are pure functions (no HTTP) so they can be unit-tested with
// fixture JSON, following the internal/incluster pattern.

func parsePVList(body []byte) ([]PV, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				CSI *struct {
					Driver       string `json:"driver"`
					VolumeHandle string `json:"volumeHandle"`
				} `json:"csi"`
				StorageClassName string `json:"storageClassName"`
				ClaimRef         *struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"claimRef"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode PersistentVolumeList: %w", err)
	}
	out := make([]PV, 0, len(list.Items))
	for _, it := range list.Items {
		pv := PV{
			Name:         it.Metadata.Name,
			Phase:        it.Status.Phase,
			StorageClass: it.Spec.StorageClassName,
		}
		if it.Spec.CSI != nil {
			pv.Driver = it.Spec.CSI.Driver
			pv.VolumeHandle = it.Spec.CSI.VolumeHandle
		}
		if cr := it.Spec.ClaimRef; cr != nil {
			pv.Claim = cr.Namespace + "/" + cr.Name
		}
		out = append(out, pv)
	}
	return out, nil
}

func parseSnapshotContentList(body []byte) ([]SnapshotContent, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Driver string `json:"driver"`
				Source struct {
					SnapshotHandle string `json:"snapshotHandle"`
				} `json:"source"`
			} `json:"spec"`
			Status *struct {
				SnapshotHandle string `json:"snapshotHandle"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode VolumeSnapshotContentList: %w", err)
	}
	out := make([]SnapshotContent, 0, len(list.Items))
	for _, it := range list.Items {
		sc := SnapshotContent{Name: it.Metadata.Name, Driver: it.Spec.Driver}
		// status.snapshotHandle once the snapshot is cut; the spec source
		// handle covers pre-provisioned contents.
		if it.Status != nil && it.Status.SnapshotHandle != "" {
			sc.SnapshotHandle = it.Status.SnapshotHandle
		} else {
			sc.SnapshotHandle = it.Spec.Source.SnapshotHandle
		}
		out = append(out, sc)
	}
	return out, nil
}

func parseNodeList(body []byte) ([]Node, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode NodeList: %w", err)
	}
	out := make([]Node, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, Node{Name: it.Metadata.Name, Labels: it.Metadata.Labels})
	}
	return out, nil
}

// parseCSINodeList keeps only the given driver's registration per node.
func parseCSINodeList(body []byte, driver string) ([]CSINode, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Drivers []struct {
					Name         string   `json:"name"`
					TopologyKeys []string `json:"topologyKeys"`
				} `json:"drivers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode CSINodeList: %w", err)
	}
	out := make([]CSINode, 0, len(list.Items))
	for _, it := range list.Items {
		cn := CSINode{Name: it.Metadata.Name}
		for _, d := range it.Spec.Drivers {
			if d.Name == driver {
				cn.HasDriver = true
				cn.TopologyKeys = d.TopologyKeys
			}
		}
		out = append(out, cn)
	}
	return out, nil
}

func parseVolumeAttachmentList(body []byte) ([]VolumeAttachment, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Attacher string `json:"attacher"`
				NodeName string `json:"nodeName"`
				Source   struct {
					PersistentVolumeName string `json:"persistentVolumeName"`
				} `json:"source"`
			} `json:"spec"`
			Status struct {
				Attached bool `json:"attached"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode VolumeAttachmentList: %w", err)
	}
	out := make([]VolumeAttachment, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, VolumeAttachment{
			Name:     it.Metadata.Name,
			Attacher: it.Spec.Attacher,
			NodeName: it.Spec.NodeName,
			PV:       it.Spec.Source.PersistentVolumeName,
			Attached: it.Status.Attached,
		})
	}
	return out, nil
}
