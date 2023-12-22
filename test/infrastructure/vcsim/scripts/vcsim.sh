#!/bin/bash

check_command() {
    eval "$1 &> /dev/null"
    if [ $? -eq 0 ]; then
        return 0
    else
        return 1
    fi
}

# Validate params
if [ -z "$1" ]
then
      echo "ERROR: VCenter name missing. usage: vcsim-prepare <vcenter-name> <cluster>"
      exit 1
fi

if [ -z "$2" ]
then
      echo "ERROR: Workload cluster name missing. usage: vcsim-prepare <vcenter-name> <cluster>"
      exit 1
fi

# Check VCenter exists or create it

check_command "kubectl get vcenter $1"
if [ $? -eq 0 ]; then
  echo "using existing VCenter $1"
else
  kubectl apply -f - &> /dev/null <<EOF
apiVersion: vcsim.infrastructure.cluster.x-k8s.io/v1alpha1
kind: VCenter
metadata:
  name: $1
EOF
  echo "created VCenter $1"
fi

# Check FakeAPIServerEndpoint exists or create it

check_command "kubectl get controlplaneendpoint $2"
if [ $? -eq 0 ]; then
  echo "using existing ControlPlaneEndpoint $2"
else
  kubectl apply -f - &> /dev/null <<EOF
apiVersion: vcsim.infrastructure.cluster.x-k8s.io/v1alpha1
kind: ControlPlaneEndpoint
metadata:
  name: $2
EOF
  echo "created ControlPlaneEndpoint $2"
  sleep 3
fi

# Check EnvSubst exists or create it

check_command "kubectl get envsubst $2"
if [ $? -eq 0 ]; then
  echo "using existing EnvSubst $2"
else
  kubectl apply -f - &> /dev/null <<EOF
apiVersion: vcsim.infrastructure.cluster.x-k8s.io/v1alpha1
kind: EnvSubst
metadata:
  name: $2
spec:
  vCenter: $1
  cluster:
    name: $2
EOF
  echo "created EnvSubst $2"
fi

i=0
maxRetry=10
while true; do
    status=$(kubectl get envsubst $2 -o json | jq ".status")
    if [ ! -z "$status" ] && [ "$status" != "null" ]; then
      break
    fi
    sleep 1
    if [ "$i" -ge "$maxRetry" ]
    then
      echo "ERROR: EnvSubst $2 is not being reconciled; check vcsim-server logs"
      exit 1
    fi
    let i++
done

# Get all the variables from EnvSubst

kubectl get envsubst $2 -o json | jq ".status.variables | to_entries | map(\"export \\(.key)=\\\"\\(.value|tostring)\\\"\") | .[]" -r > vcsim.env

echo "done!"
echo "GOVC_URL=$(kubectl get envsubst cluster1  -o json | jq -r ".status.variables.GOVC_URL")"
echo
echo "source vcsim.env"
