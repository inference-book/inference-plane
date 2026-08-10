package provisioners

// OwnsInstanceForTest exposes the deployment-owns-instance rule to the
// external test package. The rule decides whether tearing down a deployment
// may also terminate an Instance record, so it is worth testing directly
// rather than only through the teardown path.
func OwnsInstanceForTest(deployID, instanceID string) bool {
	return ownsInstance(deployID, instanceID)
}
