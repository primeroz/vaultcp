#!/bin/bash

leaf() {
    local path=$1
    local elem=$2
    local fqp=$path$elem
    local dataelems=$(vault $VAULT_READ_CMD -field=data -format=json $fqp|jq -c .)

    echo "$fqp $dataelems" >> $OUT
}

list() {
    local path=$1
    local numelems=$(vault $VAULT_LIST_CMD -format=json $path|jq .|wc -l)

    let numelems2=numelems-2
    let lastindex=numelems2-1
    for i in $(seq 0 $lastindex); do
        elem=$(vault $VAULT_LIST_CMD -format=json $path|jq ".[$i]")

        elemsans=$(eval echo $elem) #strip surrounding quotes
        case $elemsans in
        */) list $path$elemsans;;
        *) leaf $path $elemsans;;
        esac
    done
}

main() {
    WARNVER="$(vault list secret 2>&1 > /dev/null)"
    echo $WARNVER|grep "Invalid path for a versioned K/V secrets" > /dev/null

    if [[ $? = 0 ]]; then
        VAULT_LIST_CMD="kv list"
        VAULT_READ_CMD="kv get"
    else
        VAULT_LIST_CMD="list"
        VAULT_READ_CMD="read"
    fi

    OUT=/tmp/vaultls.out

    rm -f $OUT*

    echo "#!/bin/bash" > $OUT
    chmod +x $OUT

    list secret/
}

main
