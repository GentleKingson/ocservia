//! Linux host counters collected by the unprivileged Agent.
use serde_json::{Map, Value};
use std::fs;

#[derive(Default)]
pub(super) struct HostMetrics {
    previous_cpu: Option<(u64, u64)>,
}

impl HostMetrics {
    pub(super) fn sample(&mut self) -> Map<String, Value> {
        let mut values = Map::new();
        let cpu = fs::read_to_string("/proc/stat")
            .ok()
            .and_then(|s| cpu_ticks(&s));
        if let (Some((total, busy)), Some((old_total, old_busy))) = (cpu, self.previous_cpu)
            && let (Some(total), Some(busy)) =
                (total.checked_sub(old_total), busy.checked_sub(old_busy))
            && total > 0
            && busy <= total
        {
            #[allow(clippy::cast_precision_loss)]
            let ratio = busy as f64 / total as f64;
            values.insert("cpu_usage_ratio".into(), ratio.into());
        }
        self.previous_cpu = cpu;
        if let Some(used) = fs::read_to_string("/proc/meminfo")
            .ok()
            .and_then(|s| used_memory(&s))
        {
            values.insert("memory_used_bytes".into(), used.into());
        }
        if let Some((rx, tx)) = fs::read_to_string("/proc/net/dev")
            .ok()
            .and_then(|s| network_bytes(&s))
        {
            values.insert("network_rx_bytes".into(), rx.into());
            values.insert("network_tx_bytes".into(), tx.into());
        }
        values
    }
}

fn cpu_ticks(stat: &str) -> Option<(u64, u64)> {
    let mut fields = stat.lines().next()?.split_whitespace();
    if fields.next()? != "cpu" {
        return None;
    }
    // guest and guest_nice are already included in user and nice.
    let mut ticks = [0_u64; 8];
    for tick in &mut ticks {
        *tick = fields.next()?.parse().ok()?;
    }
    let total = ticks
        .iter()
        .try_fold(0_u64, |sum, tick| sum.checked_add(*tick))?;
    Some((total, total.checked_sub(ticks[3])?.checked_sub(ticks[4])?))
}

fn used_memory(info: &str) -> Option<u64> {
    let kib = |name: &str| -> Option<u64> {
        info.lines()
            .find_map(|line| line.strip_prefix(name))?
            .trim()
            .strip_suffix(" kB")?
            .trim()
            .parse()
            .ok()
    };
    kib("MemTotal:")?
        .checked_sub(kib("MemAvailable:")?)?
        .checked_mul(1024)
}

fn network_bytes(dev: &str) -> Option<(u64, u64)> {
    let mut totals = (0_u64, 0_u64);
    for line in dev.lines().skip(2) {
        let (interface, counters) = line.rsplit_once(':')?;
        if interface.trim() == "lo" {
            continue;
        }
        let mut fields = counters.split_whitespace();
        totals.0 = totals.0.checked_add(fields.next()?.parse().ok()?)?;
        totals.1 = totals.1.checked_add(fields.nth(7)?.parse().ok()?)?;
    }
    Some(totals)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn linux_counter_units_and_exclusions() {
        assert_eq!(cpu_ticks("cpu 10 1 2 5 1 0 0 1 8 0"), Some((20, 14)));
        assert_eq!(
            used_memory("MemTotal:  12 kB\nMemAvailable:  5 kB"),
            Some(7168)
        );
        assert_eq!(
            network_bytes("header\nheader\nlo: 99 0 0 0 0 0 0 0 99\neth0: 7 0 0 0 0 0 0 0 9"),
            Some((7, 9))
        );
    }
}
