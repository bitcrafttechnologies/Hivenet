// The v1 node catalog (spec section 7). Each entry supplies the palette's
// icon/label and the defaults a freshly dropped node needs to pass
// topology.Topology.Validate() -- most importantly a non-empty `image`,
// since that's the one field the server refuses to default for you.
export const NODE_CATALOG = [
  {
    type: 'host',
    label: 'Host',
    blurb: 'Alpine VM/container: hostname, IP/CIDR per interface, default gateway, DNS.',
    defaultImage: 'alpine',
    icon: 'M4 5h16v11H4z M2 19h20 M9 19v-3 M15 19v-3',
  },
  {
    type: 'router',
    label: 'Router',
    blurb: 'FRRouting image. Static routes in v1; dynamic protocols stay on the terminal tab.',
    defaultImage: 'docker.io/frrouting/frr:latest',
    icon: 'M4 17l4-10 4 10 4-10 4 10 M4 20h16',
  },
  {
    type: 'switch',
    label: 'Switch',
    blurb: 'Kernel Linux bridge, no extra packages. VLAN trunking is a v2 upgrade.',
    defaultImage: 'alpine',
    icon: 'M3 8h18M3 12h18M3 16h18',
  },
  {
    type: 'edge',
    label: 'Internet / NAT edge',
    blurb: 'Alpine + iptables/nftables. Provides NAT and acts as the topology\'s uplink node.',
    defaultImage: 'alpine',
    icon: 'M12 3a9 9 0 100 18 9 9 0 000-18z M3 12h18 M12 3c2.5 2.5 2.5 15.5 0 18 M12 3c-2.5 2.5-2.5 15.5 0 18',
  },
];

export function catalogEntry(type) {
  return NODE_CATALOG.find((c) => c.type === type);
}
