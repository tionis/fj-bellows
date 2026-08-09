package libvirt

import "encoding/xml"

const (
	metadataNamespace = "https://fj-bellows.invalid/xmlns/worker/1"
	deviceBusVirtio   = "virtio"
)

type domainXML struct {
	XMLName  xml.Name    `xml:"domain"`
	Type     string      `xml:"type,attr"`
	Name     string      `xml:"name"`
	Memory   memoryXML   `xml:"memory"`
	VCPU     int         `xml:"vcpu"`
	OS       osXML       `xml:"os"`
	Metadata metadataXML `xml:"metadata"`
	Devices  devicesXML  `xml:"devices"`
}

type memoryXML struct {
	Unit  string `xml:"unit,attr"`
	Value int    `xml:",chardata"`
}

type osXML struct {
	Firmware string    `xml:"firmware,attr,omitempty"`
	Type     osTypeXML `xml:"type"`
}

type osTypeXML struct {
	Architecture string `xml:"arch,attr,omitempty"`
	Machine      string `xml:"machine,attr,omitempty"`
	Value        string `xml:",chardata"`
}

type metadataXML struct {
	Worker workerMetadata `xml:"worker"`
}

type workerMetadata struct {
	XMLNS      string `xml:"xmlns,attr"`
	Tag        string `xml:"tag,attr"`
	CreatedAt  string `xml:"created-at,attr"`
	RootVolume string `xml:"root-volume"`
	SeedVolume string `xml:"seed-volume"`
}

type devicesXML struct {
	Disks     []diskXML    `xml:"disk"`
	Interface interfaceXML `xml:"interface"`
	Channel   channelXML   `xml:"channel"`
}

type diskXML struct {
	Type     string    `xml:"type,attr"`
	Device   string    `xml:"device,attr"`
	Driver   driverXML `xml:"driver"`
	Source   sourceXML `xml:"source"`
	Target   targetXML `xml:"target"`
	ReadOnly *struct{} `xml:"readonly,omitempty"`
}

type driverXML struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}

type sourceXML struct {
	File    string `xml:"file,attr,omitempty"`
	Network string `xml:"network,attr,omitempty"`
	Mode    string `xml:"mode,attr,omitempty"`
	Path    string `xml:"path,attr,omitempty"`
}

type targetXML struct {
	Dev string `xml:"dev,attr,omitempty"`
	Bus string `xml:"bus,attr,omitempty"`
}

type interfaceXML struct {
	Type   string    `xml:"type,attr"`
	Source sourceXML `xml:"source"`
	Model  modelXML  `xml:"model"`
}

type modelXML struct {
	Type string `xml:"type,attr"`
}

type channelXML struct {
	Type   string           `xml:"type,attr"`
	Source sourceXML        `xml:"source"`
	Target channelTargetXML `xml:"target"`
}

type channelTargetXML struct {
	Type string `xml:"type,attr"`
	Name string `xml:"name,attr"`
}

type domainPlatform struct {
	Architecture string
	Machine      string
	Firmware     string
}

func renderDomain(
	name, tag, rootVolume, rootPath, seedVolume, seedPath, network string,
	cores, memoryMB int,
	platform domainPlatform,
) ([]byte, error) {
	seedDevice := diskXML{
		Type: "file", Device: "cdrom", Driver: driverXML{Name: "qemu", Type: "raw"},
		Source: sourceXML{File: seedPath}, Target: targetXML{Dev: "sda", Bus: "sata"},
		ReadOnly: &struct{}{},
	}
	if platform.Architecture == architectureARM64 {
		// QEMU's ARM `virt` machine has no SATA controller by default. A
		// read-only virtio disk still exposes cloud-localds' CIDATA-labelled
		// filesystem to cloud-init without architecture-specific controllers.
		seedDevice.Device = "disk"
		seedDevice.Target = targetXML{Dev: "vdb", Bus: deviceBusVirtio}
	}
	domain := domainXML{
		Type:   "kvm",
		Name:   name,
		Memory: memoryXML{Unit: "MiB", Value: memoryMB},
		VCPU:   cores,
		OS: osXML{
			Firmware: platform.Firmware,
			Type: osTypeXML{
				Architecture: platform.Architecture,
				Machine:      platform.Machine,
				Value:        "hvm",
			},
		},
		Metadata: metadataXML{Worker: workerMetadata{
			XMLNS: metadataNamespace, Tag: tag, CreatedAt: nowUTC().Format(timeLayout),
			RootVolume: rootVolume, SeedVolume: seedVolume,
		}},
		Devices: devicesXML{
			Disks: []diskXML{
				{Type: "file", Device: "disk", Driver: driverXML{Name: "qemu", Type: "qcow2"}, Source: sourceXML{File: rootPath}, Target: targetXML{Dev: "vda", Bus: deviceBusVirtio}},
				seedDevice,
			},
			Interface: interfaceXML{Type: "network", Source: sourceXML{Network: network}, Model: modelXML{Type: deviceBusVirtio}},
			Channel:   channelXML{Type: "unix", Source: sourceXML{Mode: "bind"}, Target: channelTargetXML{Type: deviceBusVirtio, Name: "org.qemu.guest_agent.0"}},
		},
	}
	return xml.Marshal(domain)
}
