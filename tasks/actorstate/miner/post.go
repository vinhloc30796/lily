package miner

import (
	"bytes"
	"context"

	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lily/chain/actors/builtin/miner"
	"github.com/filecoin-project/lily/lens"
	"github.com/filecoin-project/lily/model"
	minermodel "github.com/filecoin-project/lily/model/actors/miner"
	"github.com/filecoin-project/lily/tasks/actorstate"
)

type PoStExtractor struct{}

func (PoStExtractor) Extract(ctx context.Context, a actorstate.ActorInfo, node actorstate.ActorStateAPI) (model.Persistable, error) {
	log.Debugw("extract", zap.String("extractor", "PoStExtractor"), zap.Inline(a))
	ctx, span := otel.Tracer("").Start(ctx, "PoStExtractor.Extract")
	defer span.End()
	if span.IsRecording() {
		span.SetAttributes(a.Attributes()...)
	}

	ec, err := NewMinerStateExtractionContext(ctx, a, node)
	if err != nil {
		return nil, xerrors.Errorf("creating miner state extraction context: %w", err)
	}

	// short circuit genesis state, no PoSt messages in genesis blocks.
	if !ec.HasPreviousState() {
		return nil, nil
	}
	addr := a.Address.String()
	posts := make(minermodel.MinerSectorPostList, 0)

	var partitions map[uint64]miner.Partition
	loadPartitions := func(state miner.State, epoch abi.ChainEpoch) (map[uint64]miner.Partition, error) {
		info, err := state.DeadlineInfo(epoch)
		if err != nil {
			return nil, xerrors.Errorf("deadline info: %w", err)
		}
		dline, err := state.LoadDeadline(info.Index)
		if err != nil {
			return nil, xerrors.Errorf("load deadline: %w", err)
		}
		pmap := make(map[uint64]miner.Partition)
		if err := dline.ForEachPartition(func(idx uint64, p miner.Partition) error {
			pmap[idx] = p
			return nil
		}); err != nil {
			return nil, xerrors.Errorf("foreach partition: %w", err)
		}
		return pmap, nil
	}

	processPostMsg := func(msg *lens.ExecutedMessage) error {
		sectors := make([]uint64, 0)
		if msg.Receipt == nil || msg.Receipt.ExitCode.IsError() {
			return nil
		}
		params := miner.SubmitWindowedPoStParams{}
		if err := params.UnmarshalCBOR(bytes.NewBuffer(msg.Message.Params)); err != nil {
			return xerrors.Errorf("unmarshal post params: %w", err)
		}

		var err error
		// use previous miner state and tipset state since we are using parent messages
		if partitions == nil {
			partitions, err = loadPartitions(ec.PrevState, ec.PrevTs.Height())
			if err != nil {
				return xerrors.Errorf("load partitions: %w", err)
			}
		}

		for _, p := range params.Partitions {
			all, err := partitions[p.Index].AllSectors()
			if err != nil {
				return xerrors.Errorf("all sectors: %w", err)
			}
			proven, err := bitfield.SubtractBitField(all, p.Skipped)
			if err != nil {
				return xerrors.Errorf("subtract skipped bitfield: %w", err)
			}

			if err := proven.ForEach(func(sector uint64) error {
				sectors = append(sectors, sector)
				return nil
			}); err != nil {
				return xerrors.Errorf("foreach proven: %w", err)
			}
		}

		for _, s := range sectors {
			posts = append(posts, &minermodel.MinerSectorPost{
				Height:         int64(ec.PrevTs.Height()),
				MinerID:        addr,
				SectorID:       s,
				PostMessageCID: msg.Cid.String(),
			})
		}
		return nil
	}

	tsMsgs, err := node.ExecutedAndBlockMessages(ctx, a.Current, a.Executed)
	if err != nil {
		return nil, xerrors.Errorf("getting executed and block messages: %w", err)
	}

	for _, msg := range tsMsgs.Executed {
		if msg.Message.To == a.Address && msg.Message.Method == 5 /* miner.SubmitWindowedPoSt */ {
			if err := processPostMsg(msg); err != nil {
				return nil, xerrors.Errorf("process post msg: %w", err)
			}
		}
	}
	return posts, nil
}