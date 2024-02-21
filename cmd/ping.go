package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jsdelivr/globalping-cli/globalping"
	"github.com/jsdelivr/globalping-cli/view"
	"github.com/spf13/cobra"
)

func (r *Root) initPing() {
	pingCmd := &cobra.Command{
		RunE:    r.RunPing,
		Use:     "ping [target] from [location | measurement ID | @1 | first | @-1 | last | previous]",
		GroupID: "Measurements",
		Short:   "Run a ping test",
		Long: `The ping command allows sending ping requests to a target. Often used to test the network latency and stability.

Examples:
  # Ping google.com from 2 probes in New York
  ping google.com from New York --limit 2

  # Ping google.com using probes from previous measurement
  ping google.com from rvasVvKnj48cxNjC

  # Ping google.com using probes from first measurement in session
  ping google.com from @1

  # Ping google.com using probes from last measurement in session
  ping google.com from last

  # Ping google.com using probes from second to last measurement in session
  ping google.com from @-2

  # Ping 1.1.1.1 from 2 probes from USA or Belgium with 10 packets in CI mode
  ping 1.1.1.1 from USA,Belgium --limit 2 --packets 10 --ci

  # Ping jsdelivr.com from a probe that is from the AWS network and is located in Montreal with latency output
  ping jsdelivr.com from aws+montreal --latency

  # Ping jsdelivr.com from a probe in ASN 123 with json output
  ping jsdelivr.com from 123 --json

  # Continuously ping google.com from New York
  ping google.com from New York --infinite`,
	}

	// ping specific flags
	flags := pingCmd.Flags()
	flags.IntVar(&r.ctx.Packets, "packets", 0, "Specifies the desired amount of ECHO_REQUEST packets to be sent (default 3)")
	flags.BoolVar(&r.ctx.Infinite, "infinite", false, "Keep pinging the target continuously until stopped (default false)")

	r.Cmd.AddCommand(pingCmd)
}

func (r *Root) RunPing(cmd *cobra.Command, args []string) error {
	err := r.updateContext(cmd.CalledAs(), args)
	if err != nil {
		return err
	}

	r.ctx.RecordToSession = true
	if r.ctx.Infinite {
		r.ctx.Packets = 16
	}

	opts := &globalping.MeasurementCreate{
		Type:              "ping",
		Target:            r.ctx.Target,
		Limit:             r.ctx.Limit,
		InProgressUpdates: !r.ctx.CIMode,
		Options: &globalping.MeasurementOptions{
			Packets: r.ctx.Packets,
		},
	}
	opts.Locations, err = r.getLocations()
	if err != nil {
		r.Cmd.SilenceUsage = true
		return err
	}

	if r.ctx.Infinite {
		return r.pingInfinite(opts)
	}

	hm, err := r.createMeasurement(opts)
	if err != nil {
		return err
	}
	return r.viewer.Output(hm.Id, opts)
}

func (r *Root) pingInfinite(opts *globalping.MeasurementCreate) error {
	if r.ctx.Limit > 5 {
		return fmt.Errorf("continous mode is currently limited to 5 probes")
	}

	var err error
	// Trap sigterm or interupt to display summary on exit
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		err = r.ping(opts)
		if err != nil {
			sig <- syscall.SIGINT
			return
		}
	}()
	<-sig

	if err == nil {
		r.viewer.OutputSummary()
	}
	return err
}

func (r *Root) ping(opts *globalping.MeasurementCreate) error {
	var runErr error
	mbuf := NewMeasurementsBuffer(2)
	for {
		mbuf.Restart()
		elapsedTime := time.Duration(0)
		el := mbuf.Next()
		for el != nil {
			m, err := r.client.GetMeasurement(el.Id)
			if err != nil {
				r.Cmd.SilenceUsage = true
				return err
			}
			el.Status = m.Status
			if len(m.Results) == 0 {
				el = mbuf.Next()
				continue
			}
			err = r.viewer.OutputInfinite(m)
			if err != nil {
				r.Cmd.SilenceUsage = true
				return err
			}
			if m.Status != globalping.StatusInProgress {
				mbuf.Remove(el)
			} else {
				el.IsPartiallyFinished = r.IsPartiallyFinished(m)
			}
			statuses := ""
			for i := range m.Results {
				statuses += fmt.Sprintf("%s ", m.Results[i].Result.Status)
			}
			if runErr == nil && mbuf.CanAppend() {
				opts.Locations = []globalping.Locations{{Magic: r.ctx.History.Last().Id}}
				start := r.time.Now()
				hm, err := r.createMeasurement(opts)
				if err != nil {
					runErr = err
				}
				mbuf.Append(hm)
				elapsedTime += r.time.Now().Sub(start)
			}
			el = mbuf.Next()
		}
		if mbuf.Len() > 0 {
			time.Sleep(r.ctx.APIMinInterval - elapsedTime)
			continue
		}
		if runErr != nil {
			return runErr
		}
		last := r.ctx.History.Last()
		if last != nil {
			opts.Locations = []globalping.Locations{{Magic: r.ctx.History.Last().Id}}
		}
		hm, err := r.createMeasurement(opts)
		if err != nil {
			return err
		}
		mbuf.Append(hm)
	}
}

func (r *Root) IsPartiallyFinished(m *globalping.Measurement) bool {
	if m.Status != globalping.StatusInProgress {
		return false
	}
	for i := range m.Results {
		if m.Results[i].Result.Status == globalping.StatusFinished {
			return true
		}
	}
	return false
}

func (r *Root) createMeasurement(opts *globalping.MeasurementCreate) (*view.HistoryItem, error) {
	res, showHelp, err := r.client.CreateMeasurement(opts)
	if err != nil {
		if !showHelp {
			r.Cmd.SilenceUsage = true
		}
		return nil, err
	}
	r.ctx.MeasurementsCreated++
	hm := &view.HistoryItem{
		Id:        res.ID,
		Status:    globalping.StatusInProgress,
		StartedAt: r.time.Now(),
	}
	r.ctx.History.Push(hm)
	if r.ctx.RecordToSession {
		r.ctx.RecordToSession = false
		err := saveIdToSession(res.ID)
		if err != nil {
			r.printer.Printf("Warning: %s\n", err)
		}
	}
	return hm, nil
}

type MeasurementsBuffer struct {
	capacity int
	items    []*view.HistoryItem
	pos      int
}

func NewMeasurementsBuffer(capacity int) *MeasurementsBuffer {
	return &MeasurementsBuffer{
		capacity: capacity,
		items:    make([]*view.HistoryItem, 0, capacity),
	}
}

func (b *MeasurementsBuffer) Len() int {
	return len(b.items)
}

func (b *MeasurementsBuffer) Next() *view.HistoryItem {
	if b.pos >= len(b.items) {
		return nil
	}
	b.pos++
	return b.items[b.pos-1]
}

func (b *MeasurementsBuffer) Restart() {
	b.pos = 0
}

func (b *MeasurementsBuffer) Append(hm *view.HistoryItem) {
	b.items = append(b.items, hm)
}

func (b *MeasurementsBuffer) Remove(el *view.HistoryItem) {
	if len(b.items) == 0 {
		return
	}
	newb := make([]*view.HistoryItem, 0, b.capacity)
	for i, item := range b.items {
		if item != el {
			newb = append(newb, item)
		} else if i < b.pos {
			b.pos--
		}
	}
	b.items = newb
}

func (b *MeasurementsBuffer) CanAppend() bool {
	if len(b.items) >= b.capacity {
		return false
	}
	for _, el := range b.items {
		if el.IsPartiallyFinished {
			return true
		}
	}
	return false
}
