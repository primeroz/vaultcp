# vaultcp
copy secrets between vault clusters

* A Vault secret migration code generator script.
* A source Vault is recursively listed and read and for each secret a write command will be generated to a worker script.
* A master script vaultcp.out will execute in parallel the worker scripts vaultcp.out.0 .. vaultcp.out.9
* The source vault is specified by VAULT_ADDR and an admnin token with VAULT_TOKEN env variables.
* The generated master script should have the destination Vault and token defined by different values for these env variables.
