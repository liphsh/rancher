package authz

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	typescorev1 "github.com/rancher/types/apis/core/v1"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	typesrbacv1 "github.com/rancher/types/apis/rbac.authorization.k8s.io/v1"
	"github.com/rancher/types/config"
	"k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

const (
	rtbOwnerLabel          = "authz.cluster.cattle.io/rtb-owner"
	projectIDAnnotation    = "field.cattle.io/projectId"
	prtbByProjectIndex     = "authz.cluster.cattle.io/prtb-by-project"
	prtbByProjectUserIndex = "authz.cluster.cattle.io/prtb-by-project-user"
	nsByProjectIndex       = "authz.cluster.cattle.io/ns-by-project"
	crByNSIndex            = "authz.cluster.cattle.io/cr-by-ns"
)

func Register(workload *config.ClusterContext) {
	// Add cache informer to project role template bindings
	informer := workload.Management.Management.ProjectRoleTemplateBindings("").Controller().Informer()
	indexers := map[string]cache.IndexFunc{
		prtbByProjectIndex:     prtbByProjectName,
		prtbByProjectUserIndex: prtbByProjectAndUser,
	}
	informer.AddIndexers(indexers)

	// Index for looking up namespaces by projectID annotation
	nsInformer := workload.Core.Namespaces("").Controller().Informer()
	nsIndexers := map[string]cache.IndexFunc{
		nsByProjectIndex: nsByProjectID,
	}
	nsInformer.AddIndexers(nsIndexers)

	// Get ClusterRoles by the namespaces the authorizes because they are in a project
	crInformer := workload.RBAC.ClusterRoles("").Controller().Informer()
	crIndexers := map[string]cache.IndexFunc{
		crByNSIndex: crByNS,
	}
	crInformer.AddIndexers(crIndexers)

	r := &manager{
		workload:      workload,
		prtbIndexer:   informer.GetIndexer(),
		nsIndexer:     nsInformer.GetIndexer(),
		crIndexer:     crInformer.GetIndexer(),
		rtLister:      workload.Management.Management.RoleTemplates("").Controller().Lister(),
		rbLister:      workload.RBAC.RoleBindings("").Controller().Lister(),
		crbLister:     workload.RBAC.ClusterRoleBindings("").Controller().Lister(),
		crLister:      workload.RBAC.ClusterRoles("").Controller().Lister(),
		nsLister:      workload.Core.Namespaces("").Controller().Lister(),
		clusterLister: workload.Management.Management.Clusters("").Controller().Lister(),
		clusterName:   workload.ClusterName,
	}
	workload.Management.Management.Projects("").AddClusterScopedLifecycle("project-namespace-auth", workload.ClusterName, newProjectLifecycle(r))
	workload.Management.Management.ProjectRoleTemplateBindings("").AddClusterScopedLifecycle("cluster-prtb-sync", workload.ClusterName, newPRTBLifecycle(r))
	workload.Management.Management.ClusterRoleTemplateBindings("").AddClusterScopedLifecycle("cluster-crtb-sync", workload.ClusterName, newCRTBLifecycle(r))
	workload.Management.Management.RoleTemplates("").AddClusterScopedLifecycle("cluster-roletemplate-sync", workload.ClusterName, newRTLifecycle(r))
	workload.Core.Namespaces("").AddLifecycle("namespace-auth", newNamespaceLifecycle(r))
}

type manager struct {
	workload      *config.ClusterContext
	rtLister      v3.RoleTemplateLister
	prtbIndexer   cache.Indexer
	nsIndexer     cache.Indexer
	crIndexer     cache.Indexer
	crLister      typesrbacv1.ClusterRoleLister
	crbLister     typesrbacv1.ClusterRoleBindingLister
	rbLister      typesrbacv1.RoleBindingLister
	nsLister      typescorev1.NamespaceLister
	clusterLister v3.ClusterLister
	clusterName   string
}

func (m *manager) ensureRoles(rts map[string]*v3.RoleTemplate) error {
	roleCli := m.workload.K8sClient.RbacV1().ClusterRoles()
	for _, rt := range rts {
		if rt.External {
			continue
		}

		if role, err := m.crLister.Get("", rt.Name); err == nil && role != nil {
			if reflect.DeepEqual(role.Rules, rt.Rules) {
				continue
			}
			role = role.DeepCopy()
			role.Rules = rt.Rules
			_, err := roleCli.Update(role)
			if err != nil {
				return errors.Wrapf(err, "couldn't update role %v", rt.Name)
			}
			continue
		}

		_, err := roleCli.Create(&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: rt.Name,
			},
			Rules: rt.Rules,
		})
		if err != nil {
			return errors.Wrapf(err, "couldn't create role %v", rt.Name)
		}
	}

	return nil
}

func (m *manager) gatherRoles(rt *v3.RoleTemplate, roleTemplates map[string]*v3.RoleTemplate) error {
	roleTemplates[rt.Name] = rt

	for _, rtName := range rt.RoleTemplateNames {
		subRT, err := m.rtLister.Get("", rtName)
		if err != nil {
			return errors.Wrapf(err, "couldn't get RoleTemplate %s", rtName)
		}
		if err := m.gatherRoles(subRT, roleTemplates); err != nil {
			return errors.Wrapf(err, "couldn't gather RoleTemplate %s", rtName)
		}
	}

	return nil
}

func (m *manager) ensureBindings(ns string, roles map[string]*v3.RoleTemplate, binding *v3.ProjectRoleTemplateBinding) error {
	roleBindings := m.workload.K8sClient.RbacV1().RoleBindings(ns)

	set := labels.Set(map[string]string{rtbOwnerLabel: string(binding.UID)})
	desiredRBs := map[string]*rbacv1.RoleBinding{}
	subject := buildSubjectFromPRTB(binding)
	for roleName := range roles {
		bindingName, objectMeta, subjects, roleRef := bindingParts(roleName, string(binding.UID), subject)
		desiredRBs[bindingName] = &rbacv1.RoleBinding{
			ObjectMeta: objectMeta,
			Subjects:   subjects,
			RoleRef:    roleRef,
		}
	}

	currentRBs, err := m.rbLister.List(ns, set.AsSelector())
	if err != nil {
		return err
	}
	rbsToDelete := map[string]bool{}
	processed := map[string]bool{}
	for _, rb := range currentRBs {
		// protect against an rb being in the list more than once (shouldn't happen, but just to be safe)
		if ok := processed[rb.Name]; ok {
			continue
		}
		processed[rb.Name] = true

		if _, ok := desiredRBs[rb.Name]; ok {
			delete(desiredRBs, rb.Name)
		} else {
			rbsToDelete[rb.Name] = true
		}
	}

	for _, rb := range desiredRBs {
		_, err := roleBindings.Create(rb)
		if err != nil {
			return err
		}
	}

	for name := range rbsToDelete {
		if err := roleBindings.Delete(name, &metav1.DeleteOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func bindingParts(roleName, parentUID string, subject rbacv1.Subject) (string, metav1.ObjectMeta, []rbacv1.Subject, rbacv1.RoleRef) {
	bindingName := strings.ToLower(fmt.Sprintf("%v-%v", roleName, subject.Name))
	return bindingName,
		metav1.ObjectMeta{
			Name:   bindingName,
			Labels: map[string]string{rtbOwnerLabel: parentUID},
		},
		[]rbacv1.Subject{subject},
		rbacv1.RoleRef{
			Kind: "ClusterRole",
			Name: roleName,
		}
}

func buildSubjectFromCRTB(binding *v3.ClusterRoleTemplateBinding) rbacv1.Subject {
	return rbacv1.Subject{
		Kind: "User",
		Name: binding.UserName,
	}
}

func buildSubjectFromPRTB(binding *v3.ProjectRoleTemplateBinding) rbacv1.Subject {
	return rbacv1.Subject{
		Kind: "User",
		Name: binding.UserName,
	}
}

func prtbByProjectName(obj interface{}) ([]string, error) {
	prtb, ok := obj.(*v3.ProjectRoleTemplateBinding)
	if !ok {
		return []string{}, nil
	}

	return []string{prtb.ProjectName}, nil
}

func getPRTBProjectAndUserKey(prtb *v3.ProjectRoleTemplateBinding) string {
	return prtb.ProjectName + "." + prtb.UserName
}

func prtbByProjectAndUser(obj interface{}) ([]string, error) {
	prtb, ok := obj.(*v3.ProjectRoleTemplateBinding)
	if !ok {
		return []string{}, nil
	}

	return []string{getPRTBProjectAndUserKey(prtb)}, nil
}

func nsByProjectID(obj interface{}) ([]string, error) {
	ns, ok := obj.(*v1.Namespace)
	if !ok {
		return []string{}, nil
	}

	if id, ok := ns.Annotations[projectIDAnnotation]; ok {
		return []string{id}, nil
	}

	return []string{}, nil
}
