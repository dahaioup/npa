package service

import (
	"context"
	"time"

	"github.com/npa/npa/internal/audit"
	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/policy"
	"github.com/npa/npa/internal/selfupdate"
)

// upgradeApplyTimeout 覆盖下载+校验+解包的总预算（包可达几十 MB）。
const upgradeApplyTimeout = 15 * time.Minute

// UpgradeApply 在目标节点上执行原子升级：MCP/上级只给包地址，剩下的
// 下载→校验→选平台二进制→备份→守护拉起→巡检回滚全部固化在 NPA 内部。
func (s *Service) UpgradeApply(ctx context.Context, nodeID, packageURL, sha256, expectedVersion string) (*selfupdate.ApplyResult, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, upgradeApplyTimeout)
	defer cancel()

	req := selfupdate.ApplyRequest{
		PackageURL:      packageURL,
		SHA256:          sha256,
		ExpectedVersion: expectedVersion,
	}
	apply := func() (*selfupdate.ApplyResult, error) { return selfupdate.UpgradeApply(ctx, req) }
	params := audit.SafeParameters(map[string]string{"package_url": packageURL})

	if s.isSelfNode(nodeID) {
		result, err := apply()
		s.Audit.Record(ctx, "upgrade.apply", model.NodeID(), policy.CapBase, params, started, err)
		return result, err
	}
	if s.screenTargetKind(nodeID) != "agent" {
		return nil, apperrors.New(apperrors.NotSupported, "目标不是 NPA 节点，无法远程升级")
	}
	var result selfupdate.ApplyResult
	err := s.screenPost(ctx, nodeID, "/api/upgrade/apply", req, &result)
	s.Audit.Record(ctx, "upgrade.apply", nodeID, policy.CapBase, params, started, err)
	if err != nil {
		return nil, err
	}
	return &result, nil
}
