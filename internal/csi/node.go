package csi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"io/fs"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/mount"
	"sigs.k8s.io/controller-runtime/pkg/client"

	secretsv1alpha1 "github.com/zncdata-labs/secret-operator/api/v1alpha1"
	secretbackend "github.com/zncdata-labs/secret-operator/internal/csi/backend"

	"github.com/zncdata-labs/secret-operator/pkg/pod_info"
	"github.com/zncdata-labs/secret-operator/pkg/volume"
)

var _ csi.NodeServer = &NodeServer{}

type NodeServer struct {
	mounter mount.Interface
	nodeID  string
	client  client.Client
}

func NewNodeServer(
	nodeId string,
	mounter mount.Interface,
	client client.Client,
) *NodeServer {
	return &NodeServer{
		nodeID:  nodeId,
		mounter: mounter,
		client:  client,
	}
}

func (n *NodeServer) NodePublishVolume(ctx context.Context, request *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if err := n.validateNodePublishVolumeRequest(request); err != nil {
		return nil, err
	}

	targetPath := request.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	// get the volume context
	// Default, volume context contains data:
	//   - csi.storage.k8s.io/pod.name: <pod-name>
	//   - csi.storage.k8s.io/pod.namespace: <pod-namespace>
	//   - csi.storage.k8s.io/pod.uid: <pod-uid>
	//   - csi.storage.k8s.io/serviceAccount.name: <service-account-name>
	//   - csi.storage.k8s.io/ephemeral: <true|false>
	//   - storage.kubernetes.io/csiProvisionerIdentity: <provisioner-identity>
	//   - volume.kubernetes.io/storage-provisioner: <provisioner-name>
	//   - volume.beta.kubernetes.io/storage-provisioner: <provisioner-name>
	// If you need more information about PVC, you should pass it to CreateVolumeResponse.Volume.VolumeContext
	// when called CreateVolume response in the controller side. Then use them here.
	// In this csi, we can get PVC annotations from volume context,
	// because we deliver it from controller to node already.
	// The following PVC annotations is required:
	//   - secrets.zncdata.dev/class: <secret-class-name>
	volumeSelector, err := volume.NewVolumeSelectorFromMap(request.GetVolumeContext())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if volumeSelector.Class == "" {
		return nil, status.Error(codes.InvalidArgument, "Secret class name missing in request")
	}

	secretClass := &secretsv1alpha1.SecretClass{}
	// get the secret class
	// SecretClass is cluster coped, so we don't need to specify the namespace
	if err := n.client.Get(ctx, client.ObjectKey{
		Name: volumeSelector.Class,
	}, secretClass); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	pod := &corev1.Pod{}
	// get the pod
	if err := n.client.Get(ctx, client.ObjectKey{
		Name:      volumeSelector.Pod,
		Namespace: volumeSelector.PodNamespace,
	}, pod); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	podInfo := pod_info.NewPodInfo(n.client, pod, volumeSelector)

	// get the secret data
	backend := secretbackend.NewBackend(n.client, podInfo, volumeSelector, secretClass)
	secretContent, err := backend.GetSecretData(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// mount the volume to the target path
	if err := n.mount(targetPath); err != nil {
		return nil, err
	}

	// write the secret data to the target path
	if err := n.writeData(targetPath, secretContent.Data); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := n.updatePod(ctx, pod.DeepCopy(), secretContent.ExpiresTime); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// updatePod updates the pod annotation with the secret expiration time.
// If the new expiration time is closer to the current time, update the pod annotation
// with the new expiration time. Otherwise, do nothing, meaning the pod annotation
// keeps the old expiration time.
func (n *NodeServer) updatePod(ctx context.Context, pod *corev1.Pod, expiresTime *int64) error {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	patch := client.MergeFrom(pod.DeepCopy())
	var err error
	if expiresTime == nil {
		logger.V(5).Info("Expiration time is nil, skip update pod annotation", "pod", pod.Name)
		return nil
	}

	existExpiresTime := int64(0)

	existExpiresTimeStr, found := pod.Annotations[volume.SecretZncdataExpirationTime]

	if found && existExpiresTimeStr != "" {
		existExpiresTime, err = strconv.ParseInt(existExpiresTimeStr, 10, 64)
		if err != nil {
			return err
		}
		logger.V(5).Info("Pod annotation found", "pod", pod.Name, "expiresTime", existExpiresTime)
		// if the new expiration time is closer to the current time, update the pod annotation
		// with the new expiration time. Otherwise, do nothing, meaning the pod annotation
		// keeps the old expiration time.
		if *expiresTime > existExpiresTime {
			return nil
		}

		pod.Annotations[volume.SecretZncdataExpirationTime] = strconv.FormatInt(*expiresTime, 10)
		logger.V(5).Info("Pod annotation updated", "pod", pod.Name, "expiresTime", expiresTime)
	} else {
		pod.Annotations[volume.SecretZncdataExpirationTime] = strconv.FormatInt(*expiresTime, 10)
		logger.V(5).Info("Pod annotation added", "pod", pod.Name, "expiresTime", expiresTime)
	}

	if err := n.client.Patch(ctx, pod, patch); err != nil {
		return err
	}
	logger.V(5).Info("Pod patched", "pod", pod.Name)
	return nil
}

// writeData writes the data to the target path.
// The data is a map of key-value pairs.
// The key is the file name, and the value is the file content.
func (n *NodeServer) writeData(targetPath string, data map[string]string) error {
	for name, content := range data {
		fileName := filepath.Join(targetPath, name)
		if err := os.WriteFile(fileName, []byte(content), fs.FileMode(0644)); err != nil {
			return err
		}
		logger.V(5).Info("File written", "file", fileName)
	}
	logger.V(5).Info("Data written", "target", targetPath)
	return nil
}

// mount mounts the volume to the target path.
// Mount the volume to the target path with tmpfs.
// The target path is created if it does not exist.
// The volume is mounted with the following options:
//   - noexec (no execution)
//   - nosuid (no set user ID)
//   - nodev (no device)
func (n *NodeServer) mount(targetPath string) error {
	// check if the target path exists
	// if not, create the target path
	// if exists, return error
	if exist, err := mount.PathExists(targetPath); err != nil {
		logger.Error(err, "failed to check if target path exists", "target", targetPath)
		return status.Error(codes.Internal, err.Error())
	} else if exist {
		err := errors.New("target path already exists")
		logger.Error(err, "failed to create target path", "target", targetPath)
		return status.Error(codes.Internal, err.Error())
	} else {
		if err := os.MkdirAll(targetPath, 0750); err != nil {
			logger.Error(err, "failed to create target path", "target", targetPath)
			return status.Error(codes.Internal, err.Error())
		}
	}

	opts := []string{
		"noexec",
		"nosuid",
		"nodev",
	}

	// mount the volume to the target path
	if err := n.mounter.Mount("tmpfs", targetPath, "tmpfs", opts); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	logger.V(1).Info("Volume mounted", "source", "tmpfs", "target", targetPath, "fsType", "tmpfs", "options", opts)
	return nil
}

// NodeUnpublishVolume unpublishes the volume from the node.
// unmount the volume from the target path, and remove the target path
func (n *NodeServer) NodeUnpublishVolume(ctx context.Context, request *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	// check requests
	if request.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if request.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	targetPath := request.GetTargetPath()

	// unmount the volume from the target path
	if err := n.mounter.Unmount(targetPath); err != nil {
		// FIXME: use status.Error to return error
		// return nil, status.Error(codes.Internal, err.Error())
		logger.V(0).Info("Volume not found, skip delete volume")
	}

	// remove the target path
	if err := os.RemoveAll(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (n *NodeServer) validateNodePublishVolumeRequest(request *csi.NodePublishVolumeRequest) error {
	if request.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "volume ID missing in request")
	}
	if request.GetTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	if request.GetVolumeCapability() == nil {
		return status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}

	if request.GetVolumeContext() == nil || len(request.GetVolumeContext()) == 0 {
		return status.Error(codes.InvalidArgument, "Volume context missing in request")
	}
	return nil
}

func (n *NodeServer) NodeStageVolume(ctx context.Context, request *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if len(request.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if len(request.GetStagingTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing in request")

	}

	if request.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (n *NodeServer) NodeUnstageVolume(ctx context.Context, request *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {

	if len(request.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if len(request.GetStagingTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing in request")

	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (n *NodeServer) NodeGetVolumeStats(ctx context.Context, request *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (n *NodeServer) NodeExpandVolume(ctx context.Context, request *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (n *NodeServer) NodeGetCapabilities(ctx context.Context, request *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	newCapabilities := func(cap csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
		return &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	var capabilities []*csi.NodeServiceCapability

	for _, capability := range []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
	} {
		capabilities = append(capabilities, newCapabilities(capability))
	}

	resp := &csi.NodeGetCapabilitiesResponse{
		Capabilities: capabilities,
	}

	return resp, nil

}

func (n *NodeServer) NodeGetInfo(ctx context.Context, request *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: n.nodeID,
	}, nil
}
