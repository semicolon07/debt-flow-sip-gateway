package media

import (
	"testing"

	"github.com/pion/rtcp"
)

func TestRewriteRTCPRebindsSenderAndReportSSRC(t *testing.T) {
	packets, err := RewriteRTCP([]rtcp.Packet{
		&rtcp.SenderReport{SSRC: 10, Reports: []rtcp.ReceptionReport{{SSRC: 20}}},
		&rtcp.ReceiverReport{SSRC: 10, Reports: []rtcp.ReceptionReport{{SSRC: 20}}},
		&rtcp.SourceDescription{Chunks: []rtcp.SourceDescriptionChunk{{Source: 10}}},
		&rtcp.Goodbye{Sources: []uint32{10, 20}},
	}, 10, 100, 20, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 4 {
		t.Fatalf("unexpected RTCP packet count %d", len(packets))
	}
	sender := packets[0].(*rtcp.SenderReport)
	receiver := packets[1].(*rtcp.ReceiverReport)
	description := packets[2].(*rtcp.SourceDescription)
	goodbye := packets[3].(*rtcp.Goodbye)
	if sender.SSRC != 100 || sender.Reports[0].SSRC != 200 ||
		receiver.SSRC != 100 || receiver.Reports[0].SSRC != 200 ||
		description.Chunks[0].Source != 100 || goodbye.Sources[0] != 100 || goodbye.Sources[1] != 200 {
		t.Fatalf("RTCP SSRC mapping was not applied: %#v", packets)
	}
}
