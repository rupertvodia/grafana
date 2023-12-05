import { useParams } from 'react-router-dom';
import { faNetworkWired, faSkull } from '@fortawesome/free-solid-svg-icons';

import Page from '../features/layout/Page';
import { useStreaming } from '../hooks/stream';

import styles from './Streaming.module.css';

function PageStreaming() {
  const { componentID } = useParams();
  const { data, loading, error } = useStreaming(String(componentID));

  return (
    <Page name="Debug with X-Ray" desc="Debug stream of data" icon={faSkull}>
      {loading && <p>Loading...</p>}
      {error && <p>Error: {error}</p>}
      <pre className={styles.streamingData}>{data}</pre>
    </Page>
  );
}

export default PageStreaming;
