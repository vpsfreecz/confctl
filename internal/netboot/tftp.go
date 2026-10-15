package netboot

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

type spin struct{ Name, Label string }
type menuData struct {
	Config        Config
	Machine       *Machine
	Generation    *Generation
	Spins         []spin
	SpinLabel     string
	SpinMachines  []*Machine
	Root, Timeout bool
}

func tail(m *Machine) []*Generation {
	if len(m.Generations) > 1 {
		return m.Generations[1:]
	}
	return nil
}
func stamp(g *Generation) string {
	if g.Current {
		return "current"
	}
	return g.TimeS
}
func renderMenu(source string, data menuData) ([]byte, error) {
	t, err := template.New("netboot").Funcs(template.FuncMap{"join": func(a []string) string { return strings.Join(a, " ") }, "pxe": pxe, "tail": tail, "stamp": stamp, "display": display}).Parse(source)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func BuildTFTP(c Config, root string, machines []*Machine, d Dependencies) error {
	for _, name := range []string{"pxelinux.0", "ldlinux.c32", "libcom32.c32", "libutil.c32", "memdisk", "menu.c32", "reboot.c32"} {
		if err := d.FS.Copy(join(c.Syslinux, "share/syslinux", name), join(root, name)); err != nil {
			return err
		}
	}
	for _, name := range []string{"pxelinux.cfg", "pxeserver"} {
		if err := d.FS.Mkdir(join(root, name)); err != nil {
			return err
		}
	}
	for _, m := range machines {
		for _, g := range m.Generations {
			path := join("boot", m.FQDN, g.ID.String())
			if err := d.FS.Mkdir(join(root, path)); err != nil {
				return err
			}
			for _, b := range g.Boot {
				if b.Name == "bzImage" || b.Name == "initrd" {
					if err := d.FS.Symlink(b.Path, join(root, path, b.Name)); err != nil {
						return err
					}
				}
			}
			if g.Current {
				if err := d.FS.Symlink(g.ID.String(), join(root, "boot", m.FQDN, "current")); err != nil {
					return err
				}
			}
		}
		macs, err := stringArray(m.Current.MACs, "current macs")
		if err != nil {
			return err
		}
		for _, mac := range macs {
			if err := d.FS.Symlink("../pxeserver/machines/"+m.FQDN+"/auto.cfg", join(root, "pxelinux.cfg/01-"+strings.ReplaceAll(mac, ":", "-"))); err != nil {
				return err
			}
		}
	}
	if c.Memtest != nil {
		if err := d.FS.Mkdir(join(root, "boot/memtest86")); err != nil {
			return err
		}
		if err := d.FS.Symlink(join(c.Memtest.Package, "memtest.bin"), join(root, "boot/memtest86/memtest.bin")); err != nil {
			return err
		}
	}
	if len(c.ISO) > 0 {
		if err := d.FS.Mkdir(join(root, "boot/iso-images")); err != nil {
			return err
		}
		for _, img := range c.ISO {
			if err := d.FS.Symlink(img.File, join(root, "boot/iso-images", img.Name)); err != nil {
				return err
			}
		}
	}
	spins := []spin{}
	seen := map[string]bool{}
	for _, m := range machines {
		if seen[m.Spin] {
			continue
		}
		seen[m.Spin] = true
		label := m.Spin
		if label == "nixos" {
			label = "NixOS"
		} else if label == "vpsadminos" {
			label = "vpsAdminOS"
		}
		spins = append(spins, spin{m.Spin, label})
	}
	data := menuData{Config: c, Spins: spins}
	write := func(path, source string, v menuData) error {
		b, err := renderMenu(source, v)
		if err != nil {
			return err
		}
		return d.FS.Write(join(root, path), b)
	}
	if err := write("pxelinux.cfg/default", defaultMenu, data); err != nil {
		return err
	}
	for _, s := range spins {
		v := data
		v.SpinLabel = s.Label
		for _, m := range machines {
			if m.Spin == s.Name {
				v.SpinMachines = append(v.SpinMachines, m)
			}
		}
		sort.SliceStable(v.SpinMachines, func(i, j int) bool { return v.SpinMachines[i].Name < v.SpinMachines[j].Name })
		if err := write("pxeserver/"+s.Name+".cfg", spinMenu, v); err != nil {
			return err
		}
	}
	if err := d.FS.Mkdir(join(root, "pxeserver/machines")); err != nil {
		return err
	}
	for _, m := range machines {
		path := "pxeserver/machines/" + m.FQDN
		if err := d.FS.Mkdir(join(root, path)); err != nil {
			return err
		}
		v := data
		v.Machine = m
		v.Generation = m.Current
		v.Root = true
		switch m.Spin {
		case "nixos":
			if err := write(path+"/menu.cfg", nixosMenu, v); err != nil {
				return err
			}
			if err := write(path+"/auto.cfg", nixosAuto, v); err != nil {
				return err
			}
		case "vpsadminos":
			if err := write(path+"/menu.cfg", vpsMenu, v); err != nil {
				return err
			}
			v.Timeout = true
			if err := write(path+"/auto.cfg", vpsMenu, v); err != nil {
				return err
			}
			v.Timeout = false
			if err := write(path+"/generations.cfg", generationMenu, v); err != nil {
				return err
			}
			v.Root = false
			for _, g := range m.Generations {
				v.Generation = g
				if err := write(path+"/generation-"+g.ID.String()+".cfg", vpsMenu, v); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("no machine template for spin %s", m.Spin)
		}
	}
	if len(c.ISO) > 0 {
		return write("pxeserver/iso-images.cfg", isoMenu, data)
	}
	return nil
}

const defaultMenu = `DEFAULT menu.c32
PROMPT 0
TIMEOUT 0
MENU TITLE {{.Config.HostName}}

{{range $spin := .Spins}}LABEL {{$spin.Name}}
  MENU LABEL {{$spin.Label}} >
  KERNEL menu.c32
  APPEND pxeserver/{{$spin.Name}}.cfg

{{end}}{{if .Config.ISO}}LABEL isoimages
  MENU LABEL ISO images >
  KERNEL menu.c32
  APPEND pxeserver/iso-images.cfg

{{end}}{{if .Config.Memtest}}LABEL memtest
  MENU LABEL Memtest86
  LINUX boot/memtest86/memtest.bin
  APPEND {{join .Config.Memtest.Params}}

{{end}}LABEL local_boot
  MENU LABEL Local Boot
  LOCALBOOT 0

LABEL warm_reboot
  MENU LABEL Warm Reboot
  KERNEL reboot.c32
  APPEND --warm

LABEL cold_reboot
  MENU LABEL Cold Reboot
  KERNEL reboot.c32
`

const spinMenu = `MENU TITLE {{.SpinLabel}}

{{range $m := .SpinMachines}}LABEL {{$m.FQDN}}
  MENU LABEL {{$m.Label}} >
  KERNEL menu.c32
  APPEND pxeserver/machines/{{$m.FQDN}}/menu.cfg
{{end}}
LABEL mainmenu
  MENU LABEL < Back to Main Menu
  KERNEL menu.c32
  APPEND pxelinux.cfg/default
`

const nixosMenu = `MENU TITLE {{$.Machine.Label}}

LABEL {{$.Machine.FQDN}}
  MENU LABEL {{$.Machine.Label}}
  LINUX boot/{{$.Machine.FQDN}}/{{$.Machine.Current.ID}}/bzImage
  INITRD boot/{{$.Machine.FQDN}}/{{$.Machine.Current.ID}}/initrd
  APPEND {{join (pxe $.Machine.Current)}}

{{range $g := tail .Machine}}LABEL {{$.Machine.FQDN}}-{{$g.ID}}
  MENU LABEL Gen {{$g.ID}} - {{$g.TimeS}} - {{display $g.ShortRev}}
  LINUX boot/{{$.Machine.FQDN}}/{{$g.ID}}/bzImage
  INITRD boot/{{$.Machine.FQDN}}/{{$g.ID}}/initrd
  APPEND {{join (pxe $g)}}

{{end}}{{if .Config.Memtest}}LABEL memtest
  MENU LABEL Memtest86
  LINUX boot/memtest86/memtest.bin
  APPEND {{join .Config.Memtest.Params}}

{{end}}LABEL mainmenu
  MENU LABEL < Back to Main Menu
  KERNEL menu.c32
  APPEND pxelinux.cfg/default
`

const nixosAuto = `DEFAULT {{$.Machine.FQDN}}
PROMPT 0
TIMEOUT 50

LABEL {{$.Machine.FQDN}}
  MENU LABEL {{$.Machine.Label}}
  LINUX boot/{{$.Machine.FQDN}}/{{$.Machine.Current.ID}}/bzImage
  INITRD boot/{{$.Machine.FQDN}}/{{$.Machine.Current.ID}}/initrd
  APPEND {{join (pxe $.Machine.Current)}}
`

const vpsMenu = `{{if .Timeout}}DEFAULT menu.c32
TIMEOUT 50
{{end}}MENU TITLE {{$.Machine.ShortLabel}} ({{$.Generation.ID}} - {{stamp $.Generation}} - {{display $.Generation.ShortRev}} - {{display $.Generation.KernelVersion}})

{{range $variant := .Generation.Variants}}LABEL {{$variant.Name}}
  MENU LABEL {{$variant.Label}}
  LINUX boot/{{$.Machine.FQDN}}/{{$.Generation.ID}}/bzImage
  INITRD boot/{{$.Machine.FQDN}}/{{$.Generation.ID}}/initrd
  APPEND {{join $.Generation.Params}} {{join $variant.Params}}

{{end}}LABEL {{$.Machine.FQDN}}-generations
  MENU LABEL {{if .Root}}Generations >{{else}}< Back to Generations{{end}}
  KERNEL menu.c32
  APPEND pxeserver/machines/{{$.Machine.FQDN}}/generations.cfg

{{if .Config.Memtest}}LABEL memtest
  MENU LABEL Memtest86
  LINUX boot/memtest86/memtest.bin
  APPEND {{join .Config.Memtest.Params}}

{{end}}LABEL mainmenu
  MENU LABEL < Back to Main Menu
  KERNEL menu.c32
  APPEND pxelinux.cfg/default
`

const generationMenu = `MENU TITLE {{$.Machine.Label}} - generations

{{range $g := .Machine.Generations}}LABEL generations
  MENU LABEL Gen {{$g.ID}} - {{$g.TimeS}} - {{display $g.ShortRev}} - {{display $g.KernelVersion}}
  KERNEL menu.c32
  APPEND pxeserver/machines/{{$.Machine.FQDN}}/generation-{{$g.ID}}.cfg

{{end}}
LABEL machine
  MENU LABEL < Back to {{$.Machine.Label}}
  KERNEL menu.c32
  APPEND pxeserver/machines/{{$.Machine.FQDN}}/menu.cfg

LABEL mainmenu
  MENU LABEL < Back to Main Menu
  KERNEL menu.c32
  APPEND pxelinux.cfg/default
`

const isoMenu = `MENU TITLE ISO images

{{range $i, $img := .Config.ISO}}LABEL image{{$i}}
  MENU LABEL {{$img.Label}}
  KERNEL memdisk
  APPEND iso initrd=boot/iso-images/{{$img.Name}} raw
{{end}}
LABEL mainmenu
  MENU LABEL < Back to Main Menu
  KERNEL menu.c32
  APPEND pxelinux.cfg/default
`
