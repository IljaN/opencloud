package vfs

import (
	"context"
	gateway "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	storageProvider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	typesv1beta1 "github.com/cs3org/go-cs3apis/cs3/types/v1beta1"
	"github.com/opencloud-eu/reva/v2/pkg/rgrpc/status"
	"github.com/opencloud-eu/reva/v2/pkg/rgrpc/todo/pool"
	"github.com/opencloud-eu/reva/v2/pkg/storagespace"
	"github.com/opencloud-eu/reva/v2/pkg/utils"
	"strconv"
	"strings"
)

// MakeRelativeReference returns a relative reference for the given space and path
func MakeRelativeReference(space *storageProvider.StorageSpace, relativePath string, spacesDavRequest bool) *storageProvider.Reference {
	if space.Opaque == nil || space.Opaque.Map == nil || space.Opaque.Map["path"] == nil || space.Opaque.Map["path"].Decoder != "plain" {
		return nil // not mounted
	}
	spacePath := string(space.Opaque.Map["path"].Value)
	relativeSpacePath := "."
	if strings.HasPrefix(relativePath, spacePath) {
		relativeSpacePath = utils.MakeRelativePath(strings.TrimPrefix(relativePath, spacePath))
	} else if spacesDavRequest {
		relativeSpacePath = utils.MakeRelativePath(relativePath)
	}
	return &storageProvider.Reference{
		ResourceId: space.Root,
		Path:       relativeSpacePath,
	}
}

// MakeStorageSpaceReference find a space by id and returns a relative reference
func MakeStorageSpaceReference(spaceID string, relativePath string) (storageProvider.Reference, error) {
	resourceID, err := storagespace.ParseID(spaceID)
	if err != nil {
		return storageProvider.Reference{}, err
	}
	// be tolerant about missing sharesstorageprovider id
	if resourceID.StorageId == "" && resourceID.SpaceId == utils.ShareStorageSpaceID {
		resourceID.StorageId = utils.ShareStorageProviderID
	}
	return storageProvider.Reference{
		ResourceId: &resourceID,
		Path:       utils.MakeRelativePath(relativePath),
	}, nil
}

// LookupReferenceForPath returns:
// a reference with root and relative path
// the status and error for the lookup
func LookupReferenceForPath(ctx context.Context, selector pool.Selectable[gateway.GatewayAPIClient], path string) (*storageProvider.Reference, *rpc.Status, error) {
	space, cs3Status, err := LookUpStorageSpaceForPath(ctx, selector, path)
	if err != nil || cs3Status.Code != rpc.Code_CODE_OK {
		return nil, cs3Status, err
	}
	spacePath := string(space.Opaque.Map["path"].Value) // FIXME error checks
	return &storageProvider.Reference{
		ResourceId: space.Root,
		Path:       utils.MakeRelativePath(strings.TrimPrefix(path, spacePath)),
	}, cs3Status, nil
}

// LookUpStorageSpaceForPath returns:
// the storage spaces responsible for a path
// the status and error for the lookup
func LookUpStorageSpaceForPath(ctx context.Context, selector pool.Selectable[gateway.GatewayAPIClient], path string) (*storageProvider.StorageSpace, *rpc.Status, error) {
	// TODO add filter to only fetch spaces changed in the last 30 sec?
	// TODO cache space information, invalidate after ... 5min? so we do not need to fetch all spaces?
	// TODO use ListContainerStream to listen for changes
	// retrieve a specific storage space
	lSSReq := &storageProvider.ListStorageSpacesRequest{
		Opaque: &typesv1beta1.Opaque{
			Map: map[string]*typesv1beta1.OpaqueEntry{
				"path": {
					Decoder: "plain",
					Value:   []byte(path),
				},
				"unique": {
					Decoder: "plain",
					Value:   []byte(strconv.FormatBool(true)),
				},
			},
		},
	}

	client, err := selector.Next()
	if err != nil {
		return nil, status.NewInternal(ctx, "could not select next client"), err
	}

	lSSRes, err := client.ListStorageSpaces(ctx, lSSReq)
	if err != nil || lSSRes.Status.Code != rpc.Code_CODE_OK {
		status := status.NewStatusFromErrType(ctx, "failed to lookup storage spaces", err)
		if lSSRes != nil {
			status = lSSRes.Status
		}
		return nil, status, err
	}
	switch len(lSSRes.StorageSpaces) {
	case 0:
		return nil, status.NewNotFound(ctx, "no space found"), nil
	case 1:
		return lSSRes.StorageSpaces[0], lSSRes.Status, nil
	}

	return nil, status.NewInternal(ctx, "too many spaces returned"), nil
}
