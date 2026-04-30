// 002_vector_index — Neo4j native vector index over Node.embedding.
// `$dim` is parameterized at migration time from the configured
// embedder dimensionality. similarity_function is cosine because
// memmy L2-normalizes vectors at write time.

CREATE VECTOR INDEX node_embedding_idx IF NOT EXISTS
FOR (n:Node) ON n.embedding
OPTIONS {
  indexConfig: {
    `vector.dimensions`: $dim,
    `vector.similarity_function`: 'cosine'
  }
};
