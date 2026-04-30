// 001_constraints — uniqueness + lookup indexes for memmy's core nodes.
// One Cypher statement per `;`, applied in order by Migrate().

CREATE CONSTRAINT node_tenant_id_unique IF NOT EXISTS
FOR (n:Node) REQUIRE (n.tenant, n.id) IS UNIQUE;

CREATE CONSTRAINT message_tenant_id_unique IF NOT EXISTS
FOR (m:Message) REQUIRE (m.tenant, m.id) IS UNIQUE;

CREATE CONSTRAINT tenant_id_unique IF NOT EXISTS
FOR (t:TenantInfo) REQUIRE t.id IS UNIQUE;

CREATE CONSTRAINT counter_tenant_unique IF NOT EXISTS
FOR (c:Counter) REQUIRE c.tenant IS UNIQUE;

CREATE INDEX node_tenant_idx IF NOT EXISTS
FOR (n:Node) ON (n.tenant);

CREATE INDEX message_tenant_idx IF NOT EXISTS
FOR (m:Message) ON (m.tenant);

CREATE INDEX node_tenant_source_idx IF NOT EXISTS
FOR (n:Node) ON (n.tenant, n.source_msg_id);
