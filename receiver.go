package nada

import (
	"math"
	"time"
)

type Receiver struct {
	config                         Config
	BaselineDelay                  time.Duration // d_base
	EstimatedQueuingDelay          time.Duration // d_queue
	EstimatedPacketLossRatio       float64
	EstimatedPacketECNMarkingRatio float64
	ReceivingRate                  BitsPerSecond
	LastTimestamp                  time.Time
	CurrentTimestamp               time.Time
	RecommendedRateAdaptionMode    RateAdaptionMode

	packetStream *packetStream
}

func NewReceiver(now time.Time, config Config) *Receiver {
	return &Receiver{
		config:                         config,
		BaselineDelay:                  time.Duration(1<<63 - 1),
		EstimatedPacketLossRatio:       0.0,
		EstimatedPacketECNMarkingRatio: 0.0,
		ReceivingRate:                  0.0,
		LastTimestamp:                  now,
		CurrentTimestamp:               now,
		packetStream:                   newPacketStream(config.LogWindow),
	}
}

// OnReceiveMediaPacket implements the media receive algorithm.
func (r *Receiver) OnReceiveMediaPacket(now time.Time, sent time.Time, seq uint16, ecn bool, size Bits) error {
	// obtain current timestamp t_curr from system clock
	r.CurrentTimestamp = now

	// obtain from packet header sending time stamp t_sent
	t_sent := sent

	// obtain one-way delay measurement: d_fwd = t_curr - t_sent
	d_fwd := r.CurrentTimestamp.Sub(t_sent)

	// update baseline delay: d_base = min(d_base, d_fwd)
	if d_fwd < r.BaselineDelay {
		r.BaselineDelay = d_fwd
	}

	// update queuing delay:  d_queue = d_fwd - d_base
	r.EstimatedQueuingDelay = d_fwd - r.BaselineDelay

	if err := r.packetStream.add(now, seq, ecn, size, r.EstimatedQueuingDelay > r.config.QueueingDelayThreshold); err != nil {
		return err
	}

	p_loss_inst, p_mark_inst, r_recv_inst, hasQueueingDelay := r.packetStream.prune(now)

	// update packet loss ratio estimate p_loss
	// r.config.α*p_loss_inst + (1-r.config.α)*r.EstimatedPacketLossRatio
	r.EstimatedPacketLossRatio = r.config.α*(p_loss_inst-r.EstimatedPacketLossRatio) + r.EstimatedPacketLossRatio

	// update packet marking ratio estimate p_mark
	// r.config.α*p_mark_inst + (1-r.config.α)*r.EstimatedPacketECNMarkingRatio
	r.EstimatedPacketECNMarkingRatio = r.config.α*(p_mark_inst-r.EstimatedPacketECNMarkingRatio) + r.EstimatedPacketECNMarkingRatio

	// update measurement of receiving rate r_recv
	r.ReceivingRate = r_recv_inst

	// update recommended rate adaption mode.
	if p_loss_inst == 0 && !hasQueueingDelay {
		r.RecommendedRateAdaptionMode = RateAdaptionModeAcceleratedRampUp
	} else {
		r.RecommendedRateAdaptionMode = RateAdaptionModeGradualUpdate
	}

	return nil
}

// BuildFeedbackPacket creates a new feedback packet.
func (r *Receiver) BuildFeedbackReport() *FeedbackReport {
	// calculate non-linear warping of delay d_tilde if packet loss exists
	equivalentDelay := r.equivalentDelay()

	// calculate current aggregate congestion signal x_curr
	aggregatedCongestionSignal := equivalentDelay +
		scale(r.config.ReferenceDelayMarking, math.Pow(r.EstimatedPacketECNMarkingRatio/r.config.ReferencePacketMarkingRatio, 2)) +
		scale(r.config.ReferenceDelayLoss, math.Pow(r.EstimatedPacketLossRatio/r.config.ReferencePacketLossRatio, 2))

	// determine mode of rate adaptation for sender: rmode
	rmode := r.RecommendedRateAdaptionMode

	// update t_last = t_curr
	r.LastTimestamp = r.CurrentTimestamp

	// send feedback containing values of: rmode, x_curr, and r_recv
	return &FeedbackReport{
		RecommendedRateAdaptionMode: rmode,
		AggregatedCongestionSignal:  aggregatedCongestionSignal,
		ReceivingRate:               r.ReceivingRate,
	}
}

func scale(t time.Duration, x float64) time.Duration {
	return time.Duration(float64(t) * x)
}

// d_tilde computes d_tilde as described by
//
//	           / d_queue,                   if d_queue<QTH;
//	           |
//	d_tilde = <                                           (1)
//	           |                  (d_queue-QTH)
//	           \ QTH exp(-LAMBDA ---------------) , otherwise.
//	                                 QTH
func (r *Receiver) equivalentDelay() time.Duration {
	if r.EstimatedQueuingDelay < r.config.DelayThreshold {
		return r.EstimatedQueuingDelay
	}
	scaling := math.Exp(-r.config.λ * float64((r.EstimatedQueuingDelay-r.config.DelayThreshold)/r.config.DelayThreshold))
	return scale(r.config.DelayThreshold, scaling)
}
