//go:build linux

package driver

// buildApexGen3Packet constructs the HID packet for sending pixel data to Gen 3
// Apex keyboards on Linux. hidraw with no report ID in the descriptor expects
// data without a report ID byte: [1F 81 CMD] + [pixelData].
func buildApexGen3Packet(pixelData []byte, width, height int) []byte {
	dataSize := width * height / 8
	packetSize := 2 + dataSize // CMD(2: 1F 81) + Data

	packet := make([]byte, packetSize)
	packet[0] = 0x1F
	packet[1] = 0x81

	if len(pixelData) > dataSize {
		copy(packet[2:], pixelData[:dataSize])
	} else {
		copy(packet[2:], pixelData)
	}

	return packet
}
