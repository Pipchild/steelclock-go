package driver

// ApexGen3Protocol implements the Protocol interface for newer SteelSeries Apex
// keyboard revisions (e.g. Apex Pro TKL Gen 3, PID 0x1628) whose OLED firmware
// expects a different command prefix than the original Apex Pro/7 family.
// Confirmed by reverse-engineering (community protocol docs + USB capture):
// live framebuffer writes use cmd bytes 0x1F 0x81 (not the legacy 0x61),
// same row-major MSB pixel encoding, single packet per frame, same mi_01 interface.
type ApexGen3Protocol struct{}

// BuildFramePackets builds a single HID packet for the Gen 3 Apex keyboard display.
func (p *ApexGen3Protocol) BuildFramePackets(pixelData []byte, width, height int) [][]byte {
	return [][]byte{buildApexGen3Packet(pixelData, width, height)}
}

// Interface returns the default USB interface for Gen 3 Apex keyboards.
func (p *ApexGen3Protocol) Interface() string {
	return "mi_01"
}

// DeviceFamily returns the device family name.
func (p *ApexGen3Protocol) DeviceFamily() string {
	return "Apex Keyboard (Gen 3)"
}
