# Peer-to-Peer Secure Update

This project aims to provide a framework to securely distribute system update
using peer-to-peer procotol that works in heterogeneous network environment,
in the presence of NATs and firewalls, where there is no necessarily direct
access from a management node to the devices being updated.

The framework combines several key techniques:
1. STUN-based UDP hole punching to discover and open NAT bindings
2. A gossip protocol to deliver short messages to distribute update notifications
3. BitTorrent to securely distribute the software update

This project is part of Federated RaspberryPi micro-Infrastructure Testbed - [FRuIT](https://fruit-testbed.org).


- To run the STUN server

    ```
    $ p2pupdate server --address 0.0.0.0:3478
    ```

- To run the agent

    ```
    $ p2pupdate agent -server fruit-testbed.org:3478 --address 10.0.0.5:9322
    ```