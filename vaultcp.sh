#!/bin/bash

# A Vault secret migration code generator script.
# A source Vault is recursively listed and read and for each secret a write command will be generated to a worker script.
# A master script vaultcp.out will execute in parallel the worker scripts vaultcp.out.0 .. vaultcp.out.9
# The source vault is specified by VAULT_ADDR and an admnin token with VAULT_TOKEN env variables.
# The generated master script should have the destination Vault and token defined by different values for these env variables.

leaf() {
    local path=$1
    local elem=$2
    local fqp=$path$elem
    if [[ $VER = 0 ]]; then
        local dataelems=$(vault read -field=data -format=json $fqp|jq -c .)
    else
        local dataelems=$(vault kv get -field=data -format=json $fqp|jq -c .)
    fi

    let count=count+1
    mod=$(expr $count % 10)
    OUTPUTFILE=$OUT"."$mod
    if [[ ! -f $OUTPUTFILE ]]; then
        echo "#!/bin/bash" > $OUTPUTFILE
    fi
    if [[ $VER = 0 ]]; then
        echo "echo '$dataelems'|vault write $fqp -" >> $OUTPUTFILE
    else
        echo "echo '$dataelems'|vault kv put $fqp -" >> $OUTPUTFILE
    fi
}

list() {
    local path=$1
    if [[ $VER = 0 ]]; then
        local numelems=$(vault list -format=json $path|jq .|wc -l)
    else
        local numelems=$(vault kv list -format=json $path|jq .|wc -l)
    fi

    let numelems2=numelems-2
    let lastindex=numelems2-1
    for i in $(seq 0 $lastindex); do
        if [[ $VER = 0 ]]; then
            elem=$(vault list -format=json $path|jq ".[$i]")
        else
            elem=$(vault kv list -format=json $path|jq ".[$i]")
        fi

        elemsans=$(eval echo $elem) #strip surrounding quotes
        case $elemsans in
        */) list $path$elemsans;;
        *) leaf $path $elemsans;;
        esac
    done

}

main() {
    VER=$(vault version)
    echo $VER|grep "Vault v1" > /dev/null
    if [[ "$?" = "0" ]]; then
        VER=1
    else
        VER=0
    fi

    OUT=/tmp/vaultcp.out
    let count=0

    rm -f $OUT*

    echo "#!/bin/bash" > $OUT
    chmod +x $OUT

    list secret/
    echo "# There are $count secrets over 10 files" >> $OUT

    for i in $(seq 0 9); do
        OUTPUTFILE=$OUT"."$i
        echo "$OUTPUTFILE &" >> $OUT
    done
}

main
