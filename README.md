# vaultcp

## vaultcp.go
Recursive listing and copying between Vault clusters

* Ported functionality of vaultcp.sh and vaultls.sh to golang - Faster!
* Live copy from source Vault to dest Vault (no code generation step in the middle)
* Recursive list of a source Vault produces an output file containg the path to each secret and its data value
* Support pre and post Vault v0.10 style kv api

## vaultcp.sh
Copy secrets between vault clusters

* A Vault secret migration code generator script.
* A source Vault is recursively listed and read and for each secret a write command will be generated to a worker script.
* A master script vaultcp.out will execute in parallel the worker scripts vaultcp.out.0 .. vaultcp.out.9
* The source vault is specified by VAULT_ADDR and an admnin token with VAULT_TOKEN env variables.
* The generated master script should have the destination Vault and token defined by different values for these env variables.

## vaultls.sh
Recursively list secrets under secret/
