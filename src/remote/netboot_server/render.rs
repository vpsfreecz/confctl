//! Literal PXELINUX output replacing the original ERB templates. Keep the
//! original blank lines, generation order and NixOS generations[1..] behavior.

use super::{
    NetbootServerConfig, invalid,
    model::{Generation, Machine},
    write,
};
use std::{fmt::Write as _, io, path::Path};

fn memtest(config: &NetbootServerConfig) -> String {
    config.memtest.as_ref().map_or_else(String::new,|value|format!("LABEL memtest\n  MENU LABEL Memtest86\n  LINUX boot/memtest86/memtest.bin\n  APPEND {}\n\n",value.params.join(" ")))
}
fn main_menu() -> &'static str {
    "LABEL mainmenu\n  MENU LABEL < Back to Main Menu\n  KERNEL menu.c32\n  APPEND pxelinux.cfg/default\n"
}
fn spin_label(spin: &str) -> &str {
    match spin {
        "nixos" => "NixOS",
        "vpsadminos" => "vpsAdminOS",
        other => other,
    }
}

fn default_config(config: &NetbootServerConfig, spins: &[&str]) -> String {
    let mut result = format!(
        "DEFAULT menu.c32\nPROMPT 0\nTIMEOUT 0\nMENU TITLE {}\n\n",
        config.host_name
    );
    for spin in spins {
        write!(
            result,
            "LABEL {spin}\n  MENU LABEL {} >\n  KERNEL menu.c32\n  APPEND pxeserver/{spin}.cfg\n\n",
            spin_label(spin)
        )
        .expect("string write");
    }
    if !config.iso_images.is_empty() {
        result.push_str("LABEL isoimages\n  MENU LABEL ISO images >\n  KERNEL menu.c32\n  APPEND pxeserver/iso-images.cfg\n\n");
    }
    result.push_str(&memtest(config));
    result.push_str("LABEL local_boot\n  MENU LABEL Local Boot\n  LOCALBOOT 0\n\nLABEL warm_reboot\n  MENU LABEL Warm Reboot\n  KERNEL reboot.c32\n  APPEND --warm\n\nLABEL cold_reboot\n  MENU LABEL Cold Reboot\n  KERNEL reboot.c32\n");
    result
}

fn boot(
    label: &str,
    menu_label: &str,
    machine: &Machine,
    generation: &Generation,
    params: &str,
) -> String {
    format!(
        "LABEL {label}\n  MENU LABEL {menu_label}\n  LINUX boot/{}/{}/bzImage\n  INITRD boot/{}/{}/initrd\n  APPEND {params}\n",
        machine.fqdn, generation.number, machine.fqdn, generation.number
    )
}

fn nixos(root: &Path, config: &NetbootServerConfig, machine: &Machine) -> io::Result<()> {
    let current = machine.current();
    let mut result = format!("MENU TITLE {}\n\n", machine.label);
    result.push_str(&boot(
        &machine.fqdn,
        &machine.label,
        machine,
        current,
        &current.pxe_params().join(" "),
    ));
    result.push('\n');
    // Source deliberately uses sorted generations[1..], even when current is
    // older than the newest. Do not replace this with a current exclusion.
    for generation in machine.generations.iter().skip(1) {
        result.push_str(&boot(
            &format!("{}-{}", machine.fqdn, generation.number),
            &format!(
                "Gen {} - {} - {}",
                generation.number,
                generation.time_s,
                generation.shortrev.as_deref().unwrap_or("")
            ),
            machine,
            generation,
            &generation.pxe_params().join(" "),
        ));
        result.push('\n');
    }
    result.push_str(&memtest(config));
    result.push_str(main_menu());
    write(
        root,
        &format!("pxeserver/machines/{}/menu.cfg", machine.fqdn),
        result.as_bytes(),
    )?;
    let mut auto = format!("DEFAULT {}\nPROMPT 0\nTIMEOUT 50\n\n", machine.fqdn);
    auto.push_str(&boot(
        &machine.fqdn,
        &machine.label,
        machine,
        current,
        &current.pxe_params().join(" "),
    ));
    write(
        root,
        &format!("pxeserver/machines/{}/auto.cfg", machine.fqdn),
        auto.as_bytes(),
    )
}

fn vpsadminos_generation(
    config: &NetbootServerConfig,
    machine: &Machine,
    generation: &Generation,
    is_root: bool,
    timeout: bool,
) -> String {
    let mut result = if timeout {
        "DEFAULT menu.c32\nTIMEOUT 50\n".into()
    } else {
        String::new()
    };
    writeln!(
        result,
        "MENU TITLE {} ({} - {} - {} - {})\n",
        machine.short_label,
        generation.number,
        if generation.current {
            "current"
        } else {
            &generation.time_s
        },
        generation.shortrev.as_deref().unwrap_or(""),
        generation.kernel_version.as_deref().unwrap_or("")
    )
    .expect("string write");
    for variant in &generation.variants {
        result.push_str(&boot(
            &variant.name,
            &variant.label,
            machine,
            generation,
            &format!(
                "{} {}",
                generation.params.join(" "),
                variant.params.join(" ")
            ),
        ));
        result.push('\n');
    }
    write!(result,"LABEL {}-generations\n  MENU LABEL {}\n  KERNEL menu.c32\n  APPEND pxeserver/machines/{}/generations.cfg\n\n",machine.fqdn,if is_root {"Generations >"}else{"< Back to Generations"},machine.fqdn).expect("string write");
    result.push_str(&memtest(config));
    result.push_str(main_menu());
    result
}

fn vpsadminos(root: &Path, config: &NetbootServerConfig, machine: &Machine) -> io::Result<()> {
    for (file, timeout) in [("menu", false), ("auto", true)] {
        let result = vpsadminos_generation(config, machine, machine.current(), true, timeout);
        write(
            root,
            &format!("pxeserver/machines/{}/{file}.cfg", machine.fqdn),
            result.as_bytes(),
        )?;
    }
    let mut generations = format!("MENU TITLE {} - generations\n\n", machine.label);
    for generation in &machine.generations {
        write!(generations,"LABEL generations\n  MENU LABEL Gen {} - {} - {} - {}\n  KERNEL menu.c32\n  APPEND pxeserver/machines/{}/generation-{}.cfg\n\n",generation.number,generation.time_s,generation.shortrev.as_deref().unwrap_or(""),generation.kernel_version.as_deref().unwrap_or(""),machine.fqdn,generation.number).expect("string write");
    }
    write!(generations,"\nLABEL machine\n  MENU LABEL < Back to {}\n  KERNEL menu.c32\n  APPEND pxeserver/machines/{}/menu.cfg\n\n",machine.label,machine.fqdn).expect("string write");
    generations.push_str(main_menu());
    write(
        root,
        &format!("pxeserver/machines/{}/generations.cfg", machine.fqdn),
        generations.as_bytes(),
    )?;
    for generation in &machine.generations {
        let result = vpsadminos_generation(config, machine, generation, false, false);
        write(
            root,
            &format!(
                "pxeserver/machines/{}/generation-{}.cfg",
                machine.fqdn, generation.number
            ),
            result.as_bytes(),
        )?;
    }
    Ok(())
}

pub(super) fn tftp(
    root: &Path,
    config: &NetbootServerConfig,
    machines: &[Machine],
) -> io::Result<()> {
    let mut spins = Vec::new();
    for machine in machines {
        if !spins.contains(&machine.spin.as_str()) {
            spins.push(machine.spin.as_str());
        }
    }
    write(
        root,
        "pxelinux.cfg/default",
        default_config(config, &spins).as_bytes(),
    )?;
    for spin in &spins {
        let mut result = format!("MENU TITLE {}\n\n", spin_label(spin));
        let mut selected = machines
            .iter()
            .filter(|machine| machine.spin == *spin)
            .collect::<Vec<_>>();
        selected.sort_by(|a, b| a.name.cmp(&b.name));
        for machine in selected {
            write!(result,"LABEL {}\n  MENU LABEL {} >\n  KERNEL menu.c32\n  APPEND pxeserver/machines/{}/menu.cfg\n",machine.fqdn,machine.label,machine.fqdn).expect("string write");
        }
        result.push('\n');
        result.push_str(main_menu());
        write(root, &format!("pxeserver/{spin}.cfg"), result.as_bytes())?;
    }
    for machine in machines {
        match machine.spin.as_str() {
            "nixos" => nixos(root, config, machine)?,
            "vpsadminos" => vpsadminos(root, config, machine)?,
            other => return Err(invalid(format!("unsupported netboot menu spin {other}"))),
        }
    }
    if !config.iso_images.is_empty() {
        let mut result = "MENU TITLE ISO images\n\n".to_owned();
        for (index, image) in config.iso_images.iter().enumerate() {
            write!(result,"LABEL image{index}\n  MENU LABEL {}\n  KERNEL memdisk\n  APPEND iso initrd=boot/iso-images/{} raw\n",image.label,image.name).expect("string write");
        }
        result.push('\n');
        result.push_str(main_menu());
        write(root, "pxeserver/iso-images.cfg", result.as_bytes())?;
    }
    Ok(())
}
